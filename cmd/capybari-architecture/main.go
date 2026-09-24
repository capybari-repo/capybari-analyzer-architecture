// Command capybari-architecture runs this capability on its own.
package main

import (
	architecture "github.com/capybari-repo/capybari-analyzer-architecture"
	"github.com/capybari-repo/capybari-core/standalone"
)

var version = "dev"

func main() { standalone.Main(version, architecture.New()) }
