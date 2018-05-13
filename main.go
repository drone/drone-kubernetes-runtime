package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/drone/drone-kubernetes-runtime/engine/kubernetes"
	"github.com/drone/drone-runtime/engine"
	"github.com/drone/drone-runtime/engine/plugin"
	"github.com/drone/drone-runtime/runtime"
	"github.com/drone/drone-runtime/runtime/chroot"
	"github.com/drone/drone-runtime/runtime/term"
	"github.com/drone/drone-runtime/version"
	"github.com/drone/signal"
	"github.com/mattn/go-isatty"
)

var tty = isatty.IsTerminal(os.Stdout.Fd())

func main() {
	c := struct {
		chroot  string
		plugin  string
		timeout time.Duration
		version bool

		kubeconfig string
		masterURL  string
		namespace  string
	}{}

	flag.StringVar(&c.chroot, "chroot", "", "")
	flag.StringVar(&c.plugin, "plugin", "", "")
	flag.BoolVar(&c.version, "version", false, "")
	flag.DurationVar(&c.timeout, "timeout", time.Hour, "")

	flag.StringVar(&c.kubeconfig, "kubeconfig", filepath.Join(os.Getenv("HOME"), ".kube/config"), "")
	flag.StringVar(&c.masterURL, "masterurl", "", "")
	flag.StringVar(&c.namespace, "namespace", "default", "")

	flag.Parse()

	if c.version {
		fmt.Println(version.Version)
		os.Exit(0)
	}

	var source string
	if flag.NArg() > 0 {
		source = flag.Args()[0]
	}

	config, err := engine.ParseFile(source)
	if err != nil {
		log.Fatalln(err)
	}

	var e engine.Engine
	if c.plugin == "" {
		e, err = kubernetes.New(
			kubernetes.WithConfig(c.masterURL, c.kubeconfig),
			kubernetes.WithNamespace(c.namespace),
		)
		if err != nil {
			log.Fatalln(err)
		}
	} else {
		e, err = plugin.Open(c.plugin)
		if err != nil {
			log.Fatalln(err)
		}
	}

	hooks := &runtime.Hook{}
	hooks.GotLine = term.WriteLine(os.Stdout)
	if tty {
		hooks.GotLine = term.WriteLinePretty(os.Stdout)
	}

	var fs runtime.FileSystem
	if c.chroot != "" {
		fs, err = chroot.New(c.chroot)
		if err != nil {
			log.Fatalln(err)
		}
	}

	r := runtime.New(
		runtime.WithFileSystem(fs),
		runtime.WithEngine(e),
		runtime.WithConfig(config),
		runtime.WithHooks(hooks),
	)

	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	ctx = signal.WithContext(ctx)
	defer cancel()

	err = r.Run(ctx)
	if err != nil {
		log.Fatalln(err)
	}
}
