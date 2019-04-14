// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Go-import-redirector is an HTTP server for a custom Go import domain. It responds to requests in
// a given import path root with a meta tag specifying the source repository for the ``go get''
// command and an HTML redirect to the godoc.org documentation page for that package.
//
// Usage:
//
//	go-import-redirector [-listen address] [-grace period] [-vcs sys] <import> <repo> ...
//
// Go-import-redirector listens on an address (default ``:9001'') and responds to requests for URLs
// in one of the the given import path roots with one meta tag specifying the given source
// repository for ``go get'' and another meta tag causing a redirect to the corresponding godoc.org
// documentation page.
//
// Multiple pairs of import paths and repository URLs may be specified.
//
// For example, if invoked as:
//
//	go-import-redirector 9fans.net/go https://github.com/9fans/go
//
// then the response for 9fans.net/go/acme/editinacme will include these tags:
//
//	<meta name="go-import" content="9fans.net/go git https://github.com/9fans/go">
//	<meta http-equiv="refresh" content="0; url=https://godoc.org/9fans.net/go/acme/editinacme">
//
// If both <import> and <repo> end in /*, the corresponding path element is taken from the import
// path and substituted in repo on each request. For example, if invoked as:
//
//	go-import-redirector rsc.io/* https://github.com/rsc/*
//
// then the response for rsc.io/x86/x86asm will include these tags:
//
//	<meta name="go-import" content="rsc.io/x86 git https://github.com/rsc/x86">
//	<meta http-equiv="refresh" content="0; url=https://godoc.org/rsc.io/x86/x86asm">
//
// Note that the wildcard element (x86) has been included in the Git repo path.
//
// The -listen option specifies the address to serve from (default ``:9001'').
// If the listen address begins with "unix:", then redirects are served from a Unix domain socket.
//
// The -vcs option specifies the default version control system, git, hg, or svn (default ``git'').
// This can be changed per-repo by beginning the repo URL with the VCS name followed by a plus
// (``+''), such as "git+https://github.com/name/*".
//
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"
	"golang.org/x/sys/unix"
)

var (
	listenAddr  = flag.String("listen", ":9001", "serve http on `address`")
	defaultVCS  = flag.String("vcs", "git", "set default version control `system`")
	gracePeriod = flag.Duration("grace", time.Second*5, "grace `period` for HTTP shutdowns")
)

func usage() {
	fmt.Fprintln(os.Stderr, "Usage: go-import-redirector [options] <import> <repo> ...\n")
	fmt.Fprintln(os.Stderr, "options:")
	flag.PrintDefaults()
	fmt.Fprintln(os.Stderr, "examples:")
	fmt.Fprintln(os.Stderr, "\tgo-import-redirector rsc.io/* https://github.com/rsc/*")
	fmt.Fprintln(os.Stderr, "\tgo-import-redirector 9fans.net/go https://github.com/9fans/go")
	os.Exit(2)
}

func main() {
	// log.SetFlags(0)
	log.SetPrefix("go-import-redirector: ")
	flag.Usage = usage
	flag.Parse()

	narg := flag.NArg()
	if narg < 2 || narg%2 != 0 {
		flag.Usage()
	}

	mux := http.NewServeMux()
	for i := 0; i < narg; i += 2 {
		importPath := flag.Arg(i)
		repoPath := flag.Arg(i + 1)
		redirect, err := newRedirect(importPath, repoPath)
		if err != nil {
			log.Fatalf("error creating redirect %s -> %s: %v", err)
		}
		mux.Handle(redirect.root(), redirect)
	}

	network, addr := "tcp", *listenAddr
	if strings.HasPrefix(addr, "unix:") {
		network, addr = "unix", addr[5:]
	}

	listener, err := net.Listen(network, addr)
	if err != nil {
		log.Fatalf("error creating listener")
	}
	defer listener.Close()

	server := &http.Server{
		Handler: mux,
	}

	var wg errgroup.Group
	defer func() {
		if err := wg.Wait(); err != nil {
			log.Panicf("fatal error: %v", err)
		}
	}()

	wg.Go(func() error {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, unix.SIGTERM, unix.SIGHUP)
		defer signal.Stop(sig)

		note := <-sig
		log.Printf("received signal %v; shutting down", note)

		period := *gracePeriod
		if period <= 0 {
			return server.Close()
		}

		ctx, cancel := context.WithTimeout(context.Background(), *gracePeriod)
		defer cancel()
		err := server.Shutdown(ctx)
		if err != nil {
			log.Printf("")
			return err
		}
		return nil
	})

	wg.Go(func() error {
		err := server.Serve(listener)
		if err != nil && err != http.ErrServerClosed {
			return err
		}
		return nil
	})
}

var tmpl = template.Must(template.New("main").Parse(`<!DOCTYPE html>
<html>
<head>
<meta http-equiv="Content-Type" content="text/html; charset=utf-8"/>
<meta name="go-import" content="{{.ImportRoot}} {{.VCS}} {{.VCSRoot}}">
<meta http-equiv="refresh" content="0; url=https://godoc.org/{{.ImportRoot}}{{.Suffix}}">
</head>
<body>
Redirecting to docs at <a href="https://godoc.org/{{.ImportRoot}}{{.Suffix}}">godoc.org/{{.ImportRoot}}{{.Suffix}}</a>...
</body>
</html>
`))

type data struct {
	ImportRoot string
	VCS        string
	VCSRoot    string
	Suffix     string
}

type redirectPath struct {
	wildcard   bool
	importPath string
	repo       *url.URL
	vcs        string
}

func newRedirect(importPath, repoPath string) (*redirectPath, error) {
	if !strings.Contains(repoPath, "://") {
		return nil, errors.New("repo path must be full URL")
	}
	wildcard := strings.HasSuffix(importPath, "/*")
	if wildcard != strings.HasSuffix(repoPath, "/*") {
		return nil, errors.New("either both import and repo must have /* or neither")
	}
	if wildcard {
		importPath = strings.TrimSuffix(importPath, "/*")
		repoPath = strings.TrimSuffix(repoPath, "/*")
	}

	importPath = strings.TrimSuffix(importPath, "/")
	repo, err := url.Parse(repoPath)
	if err != nil {
		return nil, err
	}

	vcs := *defaultVCS
	if sep := strings.IndexByte(repo.Scheme, '+'); sep != -1 {
		vcs, repo.Scheme = repo.Scheme[:sep], repo.Scheme[sep+1:]
	}

	r := &redirectPath{
		wildcard:   wildcard,
		importPath: importPath,
		repo:       repo,
		vcs:        vcs,
	}
	return r, nil
}

func (r *redirectPath) root() string {
	return r.importPath + "/"
}

func (r *redirectPath) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	reqPath := strings.TrimSuffix(req.Host+req.URL.Path, "/")
	var importRoot, repoRoot, suffix string
	if r.wildcard {
		if reqPath == r.importPath {
			http.Redirect(w, req, "https://godoc.org/"+r.importPath, http.StatusFound)
			return
		}
		if !strings.HasPrefix(reqPath, r.root()) {
			http.NotFound(w, req)
			return
		}
		elem := reqPath[len(r.importPath)+1:]
		if i := strings.Index(elem, "/"); i >= 0 {
			log.Print("chopping")
			elem, suffix = elem[:i], elem[i:]
		}

		importRoot = path.Join(r.importPath, elem)
		repo := *r.repo
		repo.Path = path.Join(repo.Path, elem)
		repoRoot = repo.String()
	} else {
		if reqPath != r.importPath && !strings.HasPrefix(reqPath, r.root()) {
			http.NotFound(w, req)
			return
		}
		importRoot = r.importPath
		repoRoot = r.repo.String()
		suffix = reqPath[len(r.importPath):]
	}
	d := &data{
		ImportRoot: importRoot,
		VCS:        r.vcs,
		VCSRoot:    repoRoot,
		Suffix:     suffix,
	}
	var buf bytes.Buffer
	err := tmpl.Execute(&buf, d)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Write(buf.Bytes())
}

func pong(w http.ResponseWriter, req *http.Request) {
	fmt.Fprintf(w, "pong")
}
