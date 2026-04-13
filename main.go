package main

import (
	"github.com/noony/k8s-sustain/cmd/manager"
	_ "github.com/noony/k8s-sustain/cmd/webhook" // registers the webhook subcommand
)

func main() {
	manager.Execute()
}
