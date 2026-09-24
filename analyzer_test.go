package architecture_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	architecture "github.com/capybari/capybari-analyzer-architecture"
	"github.com/capybari/capybari-core/analyzer"
	"github.com/capybari/capybari-core/analyzertest"
	"github.com/capybari/capybari-core/facts"
	"github.com/capybari/capybari-schemas"
	"gopkg.in/yaml.v3"
)

const fixtures = "../capybari-fixtures"

func TestCapabilityMetadata(t *testing.T) {
	b, err := os.ReadFile("capability.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := analyzer.ParseCapability(b); err != nil {
		t.Fatal(err)
	}
	var doc any
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if err := schemas.ValidateValue("capability.schema.json", doc); err != nil {
		t.Fatal(err)
	}
}

func TestFixtures(t *testing.T) {
	for _, name := range []string{"node-express-legacy", "python-flask-app", "go-service"} {
		t.Run(name, func(t *testing.T) {
			r := analyzertest.Run(t, architecture.New(), analyzertest.Repo(t, filepath.Join(fixtures, name)), analyzertest.Options{})
			arch := analyzertest.Fact[facts.Architecture](t, r, facts.KeyArchitecture)
			var titles []string
			for _, f := range r.Findings {
				titles = append(titles, string(f.Severity)+" "+f.Title)
			}
			analyzertest.Golden(t, name, map[string]any{"architecture": arch, "findings": titles})
		})
	}
}

func write(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for p, body := range files {
		full := filepath.Join(root, filepath.FromSlash(p))
		os.MkdirAll(filepath.Dir(full), 0o755)
		os.WriteFile(full, []byte(body), 0o644)
	}
}

func TestLayersCyclesAndNamespaces(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, map[string]string{
		// TypeScript: models (data) imports routes (interface) -> violation;
		// services <-> models cycle across directories -> medium.
		"src/routes/users.ts":   "import { UserService } from '../services/users';\nimport express from 'express';\n",
		"src/services/users.ts": "import { User } from '../models/user';\nexport class UserService {}\n",
		"src/models/user.ts":    "import { router } from '../routes/users';\nimport { UserService } from '../services/users';\nexport class User {}\n",
		// Java packages
		"java/com/acme/api/Controller.java": "package com.acme.api;\nimport com.acme.data.Repo;\nimport java.util.List;\nclass Controller {}\n",
		"java/com/acme/data/Repo.java":       "package com.acme.data;\nimport com.acme.api.Controller;\nclass Repo {}\n",
	})
	r := analyzertest.Run(t, architecture.New(), analyzertest.Repo(t, dir), analyzertest.Options{})
	arch := analyzertest.Fact[facts.Architecture](t, r, facts.KeyArchitecture)
	got := map[string][]string{}
	for _, f := range r.Findings {
		got[f.Category] = append(got[f.Category], string(f.Severity)+" "+f.Title)
	}
	if len(got["dependency-cycle"]) != 2 {
		t.Fatalf("cycles: %v (arch cycles %v)", got["dependency-cycle"], arch.Cycles)
	}
	if !strings.Contains(strings.Join(got["dependency-cycle"], "|"), "medium Circular dependency between 3 modules") {
		t.Fatalf("expected a 3-module TS cycle: %v", got["dependency-cycle"])
	}
	var sawDataToInterface bool
	for _, v := range got["layer-violation"] {
		if strings.Contains(v, "data layer depends on interface layer (src/models → src/routes)") {
			sawDataToInterface = true
		}
	}
	if !sawDataToInterface {
		t.Fatalf("layer violations: %v", got["layer-violation"])
	}
	if len(arch.External) != 1 || arch.External[0] != "express" {
		t.Fatalf("external: %v", arch.External)
	}
	if !strings.Contains(arch.Mermaid, "graph LR") || !strings.Contains(arch.Mermaid, "n_src_models") {
		t.Fatalf("mermaid: %s", arch.Mermaid)
	}
}
