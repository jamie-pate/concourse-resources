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
	"strings"

	"golang.org/x/build/gerrit"
)

var (
	authTempDir = ""
)

type authManager struct {
	cookies      string
	cookiesPath_ string

	username      string
	password      string
	digest        bool
	sshPrivateKey string
	credsPath_    string
}

func newAuthManager(source Source) *authManager {
	return &authManager{
		cookies:       source.Cookies,
		username:      source.Username,
		password:      source.Password,
		digest:        source.DigestAuth,
		sshPrivateKey: source.PrivateKey,
	}
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
	if am.credsPath_ == "" {
		if am.sshPrivateKey != "" {
			am.credsPath_, err = storePrivateKey(am.sshPrivateKey)
			// func() closure to capture error result
			// last line is 'return err' instead of 'return nil'
		} else {
			// See: https://www.kernel.org/pub/software/scm/git/docs/git-credential.html#IOFMT
			if strings.ContainsAny(am.username, "\x00\n") ||
				strings.ContainsAny(am.password, "\x00\n") {
				return "", errors.New("invalid character in username or password")
			}
			am.credsPath_, err = writeAuthTempFile(
				"concourse-gerrit-creds",
				fmt.Sprintf("username=%s\npassword=%s\n", am.username, am.password))
		}
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
		credsPath, err := am.credsPath()
		if err != nil {
			return nil, err
		}
		// TODO: replace -o StrictHostKeyChecking=no with an explicit host fingerprint!
		args["core.sshCommand"] = fmt.Sprintf("ssh -i '%v' -F /dev/null -o StrictHostKeyChecking=no", credsPath)
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
