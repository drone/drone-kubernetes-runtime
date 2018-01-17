package main

import (
	"github.com/drone/drone-kubernetes-runtime/engine/kubernetes"
	"github.com/drone/drone-runtime/engine"
)

func Engine() (engine.Engine, error) {
	return kubernetes.NewEnv()
}
