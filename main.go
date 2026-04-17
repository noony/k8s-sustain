package main

import (
	"github.com/noony/k8s-sustain/cmd/controller"
	_ "github.com/noony/k8s-sustain/cmd/dashboard" // registers the dashboard subcommand
	_ "github.com/noony/k8s-sustain/cmd/webhook"   // registers the webhook subcommand
)

func main() {
	controller.Execute()
}
