// Copyright 2020 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/google/go-github/v29/github"
	"github.com/pkg/errors"
	"golang.org/x/oauth2"
	"gopkg.in/src-d/go-git.v4"
	"gopkg.in/src-d/go-git.v4/config"
	"gopkg.in/src-d/go-git.v4/plumbing"
)

type Environment interface {
	BenchFunc() string
	CompareTarget() string

	PostErr(err string) error
	PostResults(cmps []BenchCmp) error

	Repo() *git.Repository
}

type environment struct {
	logger Logger

	benchFunc     string
	compareTarget string

	home string
}

func (e environment) BenchFunc() string     { return e.benchFunc }
func (e environment) CompareTarget() string { return e.compareTarget }

type Local struct {
	environment

	repo *git.Repository
}

func newLocalEnv(e environment) (Environment, error) {
	r, err := git.PlainOpenWithOptions(".", &git.PlainOpenOptions{DetectDotGit: true})
	if err != nil {
		return nil, err
	}
	e.logger.Println("[Local Mode]")
	e.logger.Println("Benchmarking current version versus:", e.compareTarget)
	e.logger.Println("Benchmark func regex:", e.benchFunc)
	return &Local{environment: e, repo: r}, nil
}

func (l *Local) PostErr(string) error { return nil } // Noop. We will see error anyway.

// formatNs formats ns measurements to expose a useful amount of
// precision. It mirrors the ns precision logic of testing.B.
func formatNs(ns float64) string {
	prec := 0
	switch {
	case ns < 10:
		prec = 2
	case ns < 100:
		prec = 1
	}
	return strconv.FormatFloat(ns, 'f', prec, 64)
}

func (l *Local) PostResults(cmps []BenchCmp) error {
	fmt.Println("Results:")
	Render(os.Stdout, cmps, false, false, l.compareTarget)
	return nil
}

func (l *Local) Repo() *git.Repository { return l.repo }

// TODO: Add unit test(!).
type GitHub struct {
	environment

	repo    *git.Repository
	client  *gitHubClient
	logLink string
}

func newGitHubEnv(ctx context.Context, e environment, gc *gitHubClient, workspace string) (Environment, error) {
	r, err := git.PlainCloneContext(ctx, fmt.Sprintf("%s/%s", workspace, gc.repo), false, &git.CloneOptions{
		URL:      fmt.Sprintf("https://github.com/%s/%s.git", gc.owner, gc.repo),
		Progress: os.Stdout,
		Depth:    1,
	})
	if err != nil {
		return nil, errors.Wrap(err, "git clone")
	}

	if err := os.Chdir(filepath.Join(workspace, gc.repo)); err != nil {
		return nil, errors.Wrapf(err, "changing to %s/%s dir", workspace, gc.repo)
	}

	g := &GitHub{
		environment: e,
		repo:        r,
		client:      gc,
	}

	wt, err := g.repo.Worktree()
	if err != nil {
		return nil, err
	}

	if err := r.FetchContext(ctx, &git.FetchOptions{
		RefSpecs: []config.RefSpec{
			config.RefSpec(fmt.Sprintf("+refs/pull/%d/head:refs/heads/pullrequest", gc.prNumber)),
		},
		Progress: os.Stdout,
	}); err != nil && err != git.NoErrAlreadyUpToDate {
		return nil, errors.Wrap(err, "fetch to pull request branch failed")
	}

	if err = wt.Checkout(&git.CheckoutOptions{
		Branch: plumbing.NewBranchReferenceName("pullrequest"),
	}); err != nil {
		return nil, errors.Wrap(err, "switch to pull request branch failed")
	}

	e.logger.Println("[GitHub Mode]", gc.owner, ":", gc.repo)
	e.logger.Println("Benchmarking PR -", gc.prNumber, "versus:", e.compareTarget)
	e.logger.Println("Benchmark func regex:", e.benchFunc)
	return g, nil
}

func (g *GitHub) Repo() *git.Repository { return g.repo }

type gitHubClient struct {
	owner    string
	repo     string
	prNumber int
	client   *github.Client
}

func newGitHubClient(ctx context.Context, owner, repo string, prNumber int) (*gitHubClient, error) {
	ghToken, ok := os.LookupEnv("GITHUB_TOKEN")
	if !ok {
		return nil, fmt.Errorf("GITHUB_TOKEN missing")
	}
	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: ghToken})
	tc := oauth2.NewClient(ctx, ts)
	c := gitHubClient{
		client:   github.NewClient(tc),
		owner:    owner,
		repo:     repo,
		prNumber: prNumber,
	}
	return &c, nil
}

func (c *gitHubClient) postComment(comment string) error {
	issueComment := &github.IssueComment{Body: github.String(comment)}
	_, _, err := c.client.Issues.CreateComment(context.Background(), c.owner, c.repo, c.prNumber, issueComment)
	return err
}

func (g *GitHub) PostErr(err string) error {
	if err := g.client.postComment(fmt.Sprintf("%v. Logs: %v", err, g.logLink)); err != nil {
		return errors.Wrap(err, "posting err")
	}
	return nil
}

func (g *GitHub) PostResults(cmps []BenchCmp) error {
	b := bytes.Buffer{}
	Render(&b, cmps, false, false, g.compareTarget)
	return g.client.postComment(formatCommentToMD(b.String()))
}

func formatCommentToMD(rawTable string) string {
	tableContent := strings.Split(rawTable, "\n")
	for i := 0; i <= len(tableContent)-1; i++ {
		e := tableContent[i]
		switch {
		case e == "":

		case strings.Contains(e, "old ns/op"):
			e = "| Benchmark | Old ns/op | New ns/op | Delta |"
			tableContent = append(tableContent[:i+1], append([]string{"|-|-|-|-|"}, tableContent[i+1:]...)...)

		case strings.Contains(e, "old MB/s"):
			e = "| Benchmark | Old MB/s | New MB/s | Speedup |"
			tableContent = append(tableContent[:i+1], append([]string{"|-|-|-|-|"}, tableContent[i+1:]...)...)

		case strings.Contains(e, "old allocs"):
			e = "| Benchmark | Old allocs | New allocs | Delta |"
			tableContent = append(tableContent[:i+1], append([]string{"|-|-|-|-|"}, tableContent[i+1:]...)...)

		case strings.Contains(e, "old bytes"):
			e = "| Benchmark | Old bytes | New bytes | Delta |"
			tableContent = append(tableContent[:i+1], append([]string{"|-|-|-|-|"}, tableContent[i+1:]...)...)

		default:
			// Replace spaces with "|".
			e = strings.Join(strings.Fields(e), "|")
		}
		tableContent[i] = e
	}
	return strings.Join(tableContent, "\n")

}
