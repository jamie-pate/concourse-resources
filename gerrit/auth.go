// Copyright 2017 Google Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path"
	"strings"

	"golang.org/x/build/gerrit"
)

var (
	authTempDir = ""
)

type authManager struct {
	cookies      string
	cookiesPath_ string

	username                string
	password                string
	digest                  bool
	sshPrivateKeyPassphrase string
	sshPrivateKey           string
	credsPath_              string
	sshAgentVars            []string
}

func newAuthManager(source Source) *authManager {
	return &authManager{
		cookies:                 source.Cookies,
		username:                source.Username,
		password:                source.Password,
		digest:                  source.DigestAuth,
		sshPrivateKey:           source.PrivateKey,
		sshPrivateKeyPassphrase: source.PrivateKeyPassphrase,
	}
}

func (am *authManager) sshKillAgent() {
	if len(am.sshAgentVars) > 0 {
		cmd := exec.Command("ssh-agent", "-k")
		cmd.Env = append(os.Environ(), am.sshAgentVars...)
		cmd.Run()
		am.sshAgentVars = []string{}
	}
}

func (am *authManager) sshAddKey() (err error) {
	err = nil
	// similar to https://github.com/concourse/git-resource/blob/master/assets/common.sh#L17
	if am.sshPrivateKey != "" {
		credsPath, err := storePrivateKey(am.sshPrivateKey)
		if err != nil {
			return err
		}
		// ensure that this will be cleaned up at the end
		am.credsPath_ = credsPath
		output, err := exec.Command("ssh-agent", "-s").CombinedOutput()
		if err != nil {
			return err
		}

		// -s ensures bash style variable assignments
		//keep variable assignments, remove everything else...
		//SSH_AUTH_SOCK=/tmp/ssh-ozasB2N7ff0j/agent.111798; export SSH_AUTH_SOCK;
		//SSH_AGENT_PID=111799; export SSH_AGENT_PID;
		//echo Agent pid 111799;`
		vars := []string{}
		lines := strings.Split(string(output), "\n")
		for _, s := range lines {
			assignment := strings.Split(s, ";")
			if len(assignment) > 0 && strings.Contains(assignment[0], "=") {
				vars = append(vars, assignment[0])
				envVar := strings.Split(assignment[0], "=")
				if len(envVar) >= 2 {
					os.Setenv(envVar[0], envVar[1])
				}
			}
		}
		am.sshAgentVars = vars
		if err != nil {
			return err
		}
		cmd := exec.Command("ssh-add", credsPath)
		executablePath, err := os.Executable()
		if err != nil {
			return err
		}
		vars = append(vars,
			fmt.Sprintf("GIT_SSH_PRIVATE_KEY_PASS=%s", am.sshPrivateKeyPassphrase),
			"SSH_ASKPASS_REQUIRE=force",
			fmt.Sprintf("SSH_ASKPASS=%s", path.Join(path.Dir(executablePath), "askpass.sh")),
			"DISPLAY=",
		)
		cmd.Env = append(os.Environ(),
			vars...,
		)
		_, err = cmd.CombinedOutput()
	}
	return err
}

func (am *authManager) cookiesPath() (string, error) {
	if am.cookies == "" {
		return "", nil
	}
	var err error
	if am.cookiesPath_ == "" {
		am.cookiesPath_, err = writeAuthTempFile(
			"concourse-gerrit-cookies", am.cookies)
	}
	return am.cookiesPath_, err
}

func (am *authManager) credsPath() (string, error) {
	if am.username == "" {
		return "", nil
	}

	var err error
	if am.credsPath_ == "" && am.sshPrivateKey == "" {
		// See: https://www.kernel.org/pub/software/scm/git/docs/git-credential.html#IOFMT
		if strings.ContainsAny(am.username, "\x00\n") ||
			strings.ContainsAny(am.password, "\x00\n") {
			return "", errors.New("invalid character in username or password")
		}
		am.credsPath_, err = writeAuthTempFile(
			"concourse-gerrit-creds",
			fmt.Sprintf("username=%s\npassword=%s\n", am.username, am.password))
	}
	return am.credsPath_, err
}

func (am *authManager) gerritAuth() (gerrit.Auth, error) {
	if am.username != "" {
		if am.digest {
			return gerrit.DigestAuth(am.username, am.password), nil
		} else {
			return gerrit.BasicAuth(am.username, am.password), nil
		}
	} else if am.password != "" {
		return nil, errors.New("Password specified but username is blank")
	} else if am.cookies != "" {
		cookiesPath, err := am.cookiesPath()
		if err != nil {
			return nil, err
		}
		return gerrit.GitCookieFileAuth(cookiesPath), nil
	} else {
		return gerrit.NoAuth, nil
	}
}

func (am *authManager) gitConfigArgs() (map[string]string, error) {
	args := make(map[string]string)
	if am.sshPrivateKey != "" {
		// -F /dev/null is paranoia to prevent any other ssh config from being used
		// TODO: replace -o StrictHostKeyChecking=no with an explicit host fingerprint!
		am.sshAddKey()
		args["core.sshCommand"] = "ssh -F /dev/null -o StrictHostKeyChecking=no"
	} else if am.username != "" {
		// See: https://www.kernel.org/pub/software/scm/git/docs/technical/api-credentials.html#_credential_helpers
		credsPath, err := am.credsPath()
		if err != nil {
			return nil, err
		}
		args["credential.helper"] = fmt.Sprintf("!cat %s", credsPath)
	}

	if am.cookies != "" {
		cookiesPath, err := am.cookiesPath()
		if err != nil {
			return nil, err
		}
		args["http.cookieFile"] = cookiesPath
	}

	return args, nil
}

func (am *authManager) cleanup() {
	for _, path := range []*string{&am.cookiesPath_, &am.credsPath_} {
		if *path != "" {
			err := os.Remove(*path)
			if err != nil {
				log.Printf("error removing auth temp file %q: %s", *path, err)
			}
			*path = ""
		}
	}
	if am.cookiesPath_ != "" {
		err := os.Remove(am.cookiesPath_)
		if err != nil {
			log.Printf("error removing cookies file: %s", err)
		}
		am.cookiesPath_ = ""
	}
	am.sshKillAgent()
}

func writeAuthTempFile(suffix string, contents string) (string, error) {
	f, err := ioutil.TempFile(authTempDir, suffix)
	if err != nil {
		return "", err
	}
	defer f.Close()

	_, err = f.WriteString(contents)
	if err != nil {
		return f.Name(), err
	}

	return f.Name(), nil
}

func storePrivateKey(privateKey string) (privateKeyPath string, err error) {
	// https://github.com/concourse/git-resource/blob/master/assets/common.sh#L4
	if privateKey == "" {
		return
	}
	privateKeyFile, err := ioutil.TempFile("", "gerrit-resource-private-key-*")
	if err != nil {
		err = fmt.Errorf("Error storing private key: %v", err)
		return
	}
	err = os.Chmod(privateKeyFile.Name(), 0600)
	if err != nil {
		err = fmt.Errorf("Error changing file access mode for private key: %v", err)
	}
	_, err = privateKeyFile.Write([]byte(privateKey))
	privateKeyPath = privateKeyFile.Name()
	if err != nil {
		err2 := privateKeyFile.Truncate(0)
		err2str := ""
		if err2 != nil {
			err2str = fmt.Sprintf(" %v", err2)
		}
		err = fmt.Errorf("Error writing to private key file: %v%v", err, err2str)
	}
	return
}
