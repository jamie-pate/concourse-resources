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
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/build/gerrit"

	"github.com/google/concourse-resources/internal/resource"
)

const (
	gerritVersionFilename  = ".gerrit_version.json"
	gerritPatchsetFilename = ".gerrit_patchset.json"
)

var (
	defaultFetchProtocols = []string{"http", "anonymous http"}

	// For testing
	execGit = realExecGit
)

type InParams struct {
	Fetch  *bool     `json:"fetch"`
	Sparse *[]string `json:"sparse"`
}

type PatchSetInfo struct {
	Change   int  `json:"change"`
	PatchSet int  `json:"patch_set"`
	Branch string `json:"branch"`
}

func (psi PatchSetInfo) WriteToFile(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewEncoder(f).Encode(psi)
}

func init() {
	resource.RegisterInFunc(in)
}

func in(req resource.InRequest) error {
	var src Source
	var ver Version
	var params InParams
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
	fetch := false
	if params.Fetch != nil {
		fetch = *params.Fetch
	} else if src.Fetch != nil {
		fetch = *src.Fetch
	}
	if fetch {
		fetchUrl, fetchRef, err := resolveFetchUrlRef(src, rev)
		if err != nil {
			return fmt.Errorf("could not resolve fetch args for change %q: %v", change.ID, err)
		}
		log.Printf("Fetching from %v with %v ssh key len: %v", fetchUrl, src.PrivateKeyUser, len(src.PrivateKey))

		// Prepare destination repo and checkout requested revision
		log.Printf("Checking out in %v", dir)
		err = git(dir, "init")
		if err != nil {
			return err
		}
		err = git(dir, "--version")
		if err != nil {
			return err
		}
		err = git(dir, "config", "color.ui", "always")
		if err != nil {
			return err
		}
		err = git(dir, "config", "advice.detachedHead", "false")
		if err != nil {
			return err
		}
		configArgs, err := authMan.gitConfigArgs()
		if err != nil {
			return fmt.Errorf("error getting git config args: %v", err)
		}
		for key, value := range configArgs {
			err = git(req.TargetDir(), "config", key, value)
			if err != nil {
				return err
			}
		}
		if params.Sparse != nil {
			sparseCheckoutArgs := append([]string{"sparse-checkout", "set"}, *params.Sparse...)
			err = git(dir, sparseCheckoutArgs...)
			if err != nil {
				return err
			}
		}

		err = git(dir, "remote", "add", "origin", fetchUrl)
		if err != nil {
			return err
		}

		err = git(dir, fetchFlags(src, "fetch", "origin", fetchRef)...)
		if err != nil {
			return err
		}

		err = git(dir, "checkout", "FETCH_HEAD")
		log.Printf("Git checkout %v", dir)
		if err != nil {
			return err
		}
		err = git(dir, "config", "--global", "--add", "safe.directory", dir)
		if err != nil {
			return err
		}

		log.Printf("Git skipping submodules %v", src.SkipSubmodules)
		for _, m := range src.SkipSubmodules {
			err = git(dir, "config", fmt.Sprintf("submodule.%s.update", m), "none")
			if err != nil {
				return err
			}
		}

		err = git(dir, fetchFlags(src, "submodule", "update", "--init", "--recursive")...)
		if err != nil {
			return err
		}
	} else {
		log.Printf("Writing %s", gerritVersionFilename)
		err = os.MkdirAll(dir, 0600)
		if err != nil {
			return err
		}
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
			req.AddResponseMetadata("change label",
				fmt.Sprintf("%s %+d (%s)", label, approvalInfo.Value, approvalInfo.Name))
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

	proj_and_change_num := strings.Split(ver.ChangeId, "~")
	change_num, err := strconv.Atoi(proj_and_change_num[1])
	if err != nil {
		return fmt.Errorf("Error extracting change number from changeid %s: %v", ver.ChangeId, err)
	}
	patchSetInfo := PatchSetInfo{
		Change:   change_num,
		PatchSet: rev.PatchSetNumber,
		Branch:   change.Branch,
	}
	gerritPatchsetPath := filepath.Join(dir, gerritPatchsetFilename)
	err = patchSetInfo.WriteToFile(gerritPatchsetPath)
	if err != nil {
		return fmt.Errorf("error writing %q: %v", gerritPatchsetPath, err)
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

func fetchFlags(src Source, flags ...string) []string {
	if src.Depth > 0 {
		flags = append(flags, fmt.Sprintf("--depth=%v", src.Depth))
	}
	return flags
}

func resolveFetchUrlRef(src Source, rev *gerrit.RevisionInfo) (url, ref string, err error) {
	url = src.FetchUrl
	if src.PrivateKeyUser != "" {
		if !strings.HasPrefix(url, "ssh://") {
			return "", "", fmt.Errorf("FetchUrl '%v' is not an ssh url, but PrivateKeyUser was set", url)
		}
		parts := strings.SplitAfterN(url, "ssh://", 2)
		if len(parts) != 2 {
			return "", "", fmt.Errorf(
				"Unable to split fetchUrl %v to insert the privateKeyUser, got the wrong length: %v for %v",
				url,
				len(parts),
				parts,
			)
		}
		url = fmt.Sprintf("%s%s@%s", parts[0], src.PrivateKeyUser, parts[1])
	}
	ref = rev.Ref
	if url == "" {
		fetchProtocol := src.FetchProtocol
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
