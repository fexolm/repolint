package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"math"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"github.com/google/go-github/github"
	"golang.org/x/oauth2"
)

type linter struct {
	token   string
	user    string
	repos   []string
	ctx     context.Context
	client  *github.Client
	verbose bool

	tmpfiles []string
}

type fileChecker interface {
	CheckFile(filename string) []error
}

type brokenLinksChecker struct{}

type misspellChecker struct{}

func (c *misspellChecker) CheckFile(filename string) []error {
	out, err := exec.Command("misspell", "-error", "true", filename).CombinedOutput()
	if err != nil {
		lines := strings.Split(string(out), "\n")
		var errs []error
		for _, l := range lines {
			if l == "" {
				continue
			}
			errs = append(errs, errors.New(l))
		}
		return errs
	}
	return nil
}

func (c *brokenLinksChecker) CheckFile(filename string) []error {
	out, err := exec.Command("liche", filename).CombinedOutput()
	if err != nil {
		lines := strings.Split(string(out), "\n")
		var errs []error
		for i := 0; i < len(lines); i++ {
			l := lines[i]
			if !strings.Contains(l, "ERROR") {
				continue
			}
			// Next line contains error info.
			url := strings.TrimLeft(l, "\t ERROR")
			i++
			l = lines[i]
			if strings.Contains(l, "no such file") || strings.Contains(l, "root directory is not specified") {
				// Not interested in file lookups, since we're
				// not doing real git cloning.
				continue
			}
			errs = append(errs, errors.New(url+": "+strings.TrimSpace(l)))
		}
		return errs
	}
	return nil
}

func main() {
	log.SetFlags(0)
	var l linter

	defer func() {
		for _, f := range l.tmpfiles {
			_ = os.Remove(f)
		}
	}()

	var steps = []struct {
		name string
		fn   func() error
	}{
		{"parse flags", l.parseFlags},
		{"read token", l.readToken},
		{"init client", l.initClient},
		{"get repos list", l.getReposList},
		{"lint repos", l.lintRepos},
	}

	for _, step := range steps {
		if err := step.fn(); err != nil {
			log.Fatalf("%s: %v", step.name, err)
		}
	}
}

func (l *linter) readToken() error {
	token := os.Getenv("TOKEN")
	if token != "" {
		l.token = token
		return nil
	}
	data, err := ioutil.ReadFile("./token")
	if err != nil {
		return fmt.Errorf("no TOKEN env var and can't read token file: %v", err)
	}
	l.token = strings.TrimSpace(string(data))
	return nil
}

func (l *linter) parseFlags() error {
	flag.StringVar(&l.user, "user", "",
		`github user/organization name`)
	flag.BoolVar(&l.verbose, "v", false,
		`verbose mode that turns on additional debug output`)

	flag.Parse()

	if l.user == "" {
		return errors.New("-user argument can't be empty")
	}

	return nil
}

func (l *linter) initClient() error {
	l.ctx = context.Background()

	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: l.token})
	tc := oauth2.NewClient(l.ctx, ts)
	l.client = github.NewClient(tc)

	return nil
}

func newRepositoryListOptions() *github.RepositoryListOptions {
	// Use some high value, github will limit it anyway,
	// but we're interested in getting more data per one request.
	return &github.RepositoryListOptions{
		ListOptions: github.ListOptions{PerPage: math.MaxInt32},
	}
}

func (l *linter) getReposList() error {
	// TODO: collect only repos that were updated at least 6 months ago?

	opts := newRepositoryListOptions()
	for {
		repos, resp, err := l.client.Repositories.List(l.ctx, l.user, opts)
		if err != nil {
			return fmt.Errorf("list repos (page=%d): %v", opts.Page, err)
		}

		if l.verbose {
			log.Printf("\tdebug: fetched %d repo names\n", len(repos))
		}
		for _, repo := range repos {
			l.repos = append(l.repos, *repo.Name)
		}

		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}

	return nil
}

func (l *linter) lintRepos() error {
	for _, repo := range l.repos {
		log.Printf("\tchecking %s/%s...", l.user, repo)
		l.lintRepo(repo)
	}
	return nil
}

func (l *linter) lintRepo(repo string) {
	l.lintReadme(repo)
	l.lintFiles(repo)
}

func (l *linter) lintFilenames(repo string, list []*github.RepositoryContent) {
	// TODO: don't compile regular expressions for every call of lintFilenames.
	patterns := []struct {
		name string
		re   *regexp.Regexp
	}{
		{"Vim swap", regexp.MustCompile(`^.*\.swp$`)},
		{"Emacs autosave", regexp.MustCompile(`^#.*#$`)},
		{"Emacs backup", regexp.MustCompile(`^.*~$`)},
	}

	for _, f := range list {
		for _, pat := range patterns {
			if pat.re.MatchString(*f.Name) {
				log.Printf("%s: remove %s file: %s", repo, pat.name, *f.Name)
				break // Can't match more than 1 kind
			}
		}
	}
}

func (l *linter) lintFiles(repo string) {
	// TODO: recurse into sub-directories.
	// TODO: do multi-request if there is more files than github returns per 1 req.
	_, list, _, err := l.client.Repositories.GetContents(l.ctx, l.user, repo, "/", nil)
	if err != nil {
		panic(fmt.Sprintf("%s: list directory: %v", repo, err))
	}
	l.lintFilenames(repo, list)
}

func (l *linter) lintReadme(repo string) {
	checks := []struct {
		name    string
		checker fileChecker
	}{
		{"broken link", &brokenLinksChecker{}},
		{"misspell", &misspellChecker{}},
	}

	f, _, err := l.client.Repositories.GetReadme(l.ctx, l.user, repo, nil)
	if err != nil {
		log.Printf("%s: can't access README", repo)
		return
	}
	readme, err := f.GetContent()
	if err != nil {
		log.Fatalf("get content: %v", err)
	}
	tmp := l.newTmpFile("README*.md", []byte(readme))

	for _, c := range checks {
		for _, err := range c.checker.CheckFile(tmp.Name()) {
			log.Printf("%s: %s: %v", repo, c.name, err)
		}
	}

}

func (l *linter) newTmpFile(pattern string, data []byte) *os.File {
	f, err := ioutil.TempFile("", pattern)
	if err != nil {
		panic(fmt.Errorf("create temp file: %v", err))
	}
	_, err = f.Write(data)
	if err != nil {
		panic(fmt.Errorf("write to temp file (%s): %v", err, f.Name()))
	}
	l.tmpfiles = append(l.tmpfiles, f.Name())
	return f
}
