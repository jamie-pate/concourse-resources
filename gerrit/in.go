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
	"context"
	"fmt"
	"io/ioutil"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	"golang.org/x/build/gerrit"

	"github.com/google/concourse-resources/internal/resource"
)

const (
	gerritVersionFilename = ".gerrit_version.json"
)

var (
	defaultFetchProtocols = []string{"http", "anonymous http"}

	// For testing
	execGit = realExecGit
)

type inParams struct {
	FetchProtocol  string `json:"fetch_protocol"`
	FetchUrl       string `json:"fetch_url"`
	PrivateKey     string `json:"private_key"`
	PrivateKeyUser string `json:"private_key_user"`
	Depth          int    `json:"depth"`
}

func init() {
	resource.RegisterInFunc(in)
}

func in(req resource.InRequest) error {
	var src Source
	var ver Version
	var params inParams
	err := req.Decode(&src, &ver, &params)
	if err != nil {
		return err
	}
	dir := req.TargetDir()

	authMan := newAuthManager(src)
	defer authMan.cleanup()

	c, err := gerritClient(src, authMan)
	if err != nil {
		return fmt.Errorf("error setting up gerrit client: %v", err)
	}

	ctx := context.Background()

	// Fetch requested version from Gerrit
	change, rev, err := getVersionChangeRevision(c, ctx, ver, "CURRENT_COMMIT", "DETAILED_LABELS")
	if err != nil {
		return err
	}

	fetchUrl, fetchRef, err := resolveFetchUrlRef(params, rev)
	if err != nil {
		return fmt.Errorf("could not resolve fetch args for change %q: %v", change.ID, err)
	}

	// Prepare destination repo and checkout requested revision
	log.Printf("Checking out in %v", dir)
	err = git(dir, "init")
	if err != nil {
		return err
	}
	err = git(dir, "config", "color.ui", "always")
	if err != nil {
		return err
	}
	privateKeyPath, err := storePrivateKey(params)
	// func() closure to capture error result
	// last line is 'return err' instead of 'return nil'
	defer func() {
		err = erasePrivateKey(privateKeyPath)
	}()
	configArgs, err := authMan.gitConfigArgs()
	if err != nil {
		return fmt.Errorf("error getting git config args: %v", err)
	}
	if privateKeyPath != "" {
		// -F /dev/null is paranoia to prevent any other ssh config from being used
		configArgs["core.sshCommand"] = fmt.Sprintf("ssh -i '%v' -F /dev/null", privateKeyPath)
	}
	for key, value := range configArgs {
		err = git(req.TargetDir(), "config", key, value)
		if err != nil {
			return err
		}
	}

	err = git(dir, "remote", "add", "origin", fetchUrl)
	if err != nil {
		return err
	}

	err = git(dir, fetchFlags(params, "fetch", "origin", fetchRef)...)
	if err != nil {
		return err
	}

	err = git(dir, "checkout", "FETCH_HEAD")
	log.Printf("Git checkout %v", dir)
	if err != nil {
		return err
	}
	err = git(dir, fetchFlags(params, "submodule", "update", "--init", "--recursive")...)
	if err != nil {
		return err
	}

	// Build response metadata
	req.AddResponseMetadata("project", change.Project)
	req.AddResponseMetadata("branch", change.Branch)
	req.AddResponseMetadata("change subject", change.Subject)

	if change.Owner != nil {
		req.AddResponseMetadata("change owner",
			fmt.Sprintf("%s <%s>", change.Owner.Name, change.Owner.Email))
	}

	for label, labelInfo := range change.Labels {
		for _, approvalInfo := range labelInfo.All {
			if approvalInfo.Value != 0 {
				req.AddResponseMetadata("change label",
					fmt.Sprintf("%s %+d (%s)", label, approvalInfo.Value, approvalInfo.Name))
			}
		}
	}

	req.AddResponseMetadata("revision created", rev.Created.Time().String())

	if rev.Uploader != nil {
		req.AddResponseMetadata("revision uploader",
			fmt.Sprintf("%s <%s>", rev.Uploader.Name, rev.Uploader.Email))
	}

	link, err := buildRevisionLink(src, change.ChangeNumber, rev.PatchSetNumber)
	if err == nil {
		req.AddResponseMetadata("revision link", link)
	} else {
		log.Printf("error building revision link: %v", err)
	}

	req.AddResponseMetadata("commit id", ver.Revision)

	if rev.Commit != nil {
		req.AddResponseMetadata("commit author",
			fmt.Sprintf("%s <%s>", rev.Commit.Author.Name, rev.Commit.Author.Email))

		req.AddResponseMetadata("commit subject", rev.Commit.Subject)

		for _, parent := range rev.Commit.Parents {
			req.AddResponseMetadata("commit parent", parent.CommitID)
		}

		req.AddResponseMetadata("commit message", rev.Commit.Message)
	}

	// Write gerrit_version.json
	gerritVersionPath := filepath.Join(dir, gerritVersionFilename)
	err = ver.WriteToFile(gerritVersionPath)
	if err != nil {
		return fmt.Errorf("error writing %q: %v", gerritVersionPath, err)
	}

	// Ignore gerrit_version.json file in repo
	excludePath := filepath.Join(dir, ".git", "info", "exclude")
	excludeErr := os.MkdirAll(filepath.Dir(excludePath), 0755)
	if excludeErr == nil {
		f, excludeErr := os.OpenFile(excludePath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0644)
		if excludeErr == nil {
			defer f.Close()
			_, excludeErr = fmt.Fprintf(f, "\n/%s\n", gerritVersionFilename)
		}
	}
	if excludeErr != nil {
		log.Printf("error adding %q to %q: %v", gerritVersionPath, excludePath, excludeErr)
	}

	return err
}

func fetchFlags(params inParams, flags ...string) []string {
	if params.Depth > 0 {
		flags = append(flags, "--depth=%v")
	}
	return flags
}

func erasePrivateKey(privateKeyPath string) (err error) {
	if privateKeyPath == "" {
		return nil
	}
	return os.Remove(privateKeyPath)
}

func storePrivateKey(params inParams) (privateKeyPath string, err error) {
	// https://github.com/concourse/git-resource/blob/master/assets/common.sh#L4
	if params.PrivateKey == "" {
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
	_, err = privateKeyFile.Write([]byte(params.PrivateKey))
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

func resolveFetchUrlRef(params inParams, rev *gerrit.RevisionInfo) (url, ref string, err error) {
	url = params.FetchUrl
	if params.PrivateKeyUser != "" {
		if !strings.HasPrefix(url, "ssh://") {
			return "", "", fmt.Errorf("FetchUrl '%v' is not an ssh url, but PrivateKeyUser was set", url)
		}
		parts := strings.SplitAfterN(url, "ssh://", 1)
		if len(parts) != 2 {
			return "", "", fmt.Errorf(
				"Unable to split fetchUrl %v to insert the privateKeyUser, got the wrong length: %v for %v",
				url,
				len(parts),
				parts,
			)
		}
		url = fmt.Sprintf("%s%s@%s", parts[0], params.PrivateKeyUser, parts[1])
	}
	ref = rev.Ref
	if url == "" {
		fetchProtocol := params.FetchProtocol
		if fetchProtocol == "" {
			for _, proto := range defaultFetchProtocols {
				if _, ok := rev.Fetch[proto]; ok {
					fetchProtocol = proto
					break
				}
			}
		}
		fetchInfo, ok := rev.Fetch[fetchProtocol]
		if ok {
			url = fetchInfo.URL
			ref = fetchInfo.Ref
		} else {
			err = fmt.Errorf("no fetch info for protocol %q", fetchProtocol)
		}
	}
	return
}

func git(dir string, args ...string) error {
	gitArgs := append([]string{"-C", dir}, args...)
	log.Printf("git %v", gitArgs)
	output, err := execGit(gitArgs...)
	log.Printf("git output:\n%s", output)
	if err != nil {
		err = fmt.Errorf("git failed: %v", err)
	}
	return err
}

func realExecGit(args ...string) ([]byte, error) {
	return exec.Command("git", args...).CombinedOutput()
}

func buildRevisionLink(src Source, changeNum int, psNum int) (string, error) {
	srcUrl, err := url.Parse(src.Url)
	if err != nil {
		return "", err
	}
	srcUrl.Path = path.Join(srcUrl.Path, fmt.Sprintf("c/%d/%d", changeNum, psNum))
	return srcUrl.String(), nil
}
