module github.com/capybari/capybari-analyzer-architecture

go 1.27.1

require (
	github.com/capybari/capybari-core v0.0.0
	github.com/capybari/capybari-schemas v0.0.0
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/santhosh-tekuri/jsonschema/v6 v6.0.3 // indirect
	golang.org/x/net v0.59.0 // indirect
	golang.org/x/text v0.42.0 // indirect
)

replace github.com/capybari/capybari-core => ../capybari-core

replace github.com/capybari/capybari-schemas => ../capybari-schemas
