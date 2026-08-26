module github.com/firstboot-io/firstboot-cli

go 1.25.7

// firstboot-go is not published yet; it is checked out as a sibling of this
// repository, exactly as the workspace layout describes. Remove this the day
// the SDK carries a tag.
replace github.com/firstboot-io/firstboot-go => ../firstboot-go

require (
	github.com/BurntSushi/toml v1.6.0
	github.com/firstboot-io/firstboot-go v0.0.0-00010101000000-000000000000
	github.com/google/uuid v1.6.0
	github.com/spf13/cobra v1.10.2
	github.com/zalando/go-keyring v0.2.8
	golang.org/x/term v0.45.0
	sigs.k8s.io/yaml v1.6.0
)

require (
	github.com/apapsch/go-jsonmerge/v2 v2.0.0 // indirect
	github.com/danieljoos/wincred v1.2.3 // indirect
	github.com/godbus/dbus/v5 v5.2.2 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/oapi-codegen/runtime v1.7.0 // indirect
	github.com/spf13/pflag v1.0.9 // indirect
	go.yaml.in/yaml/v2 v2.4.2 // indirect
	golang.org/x/sys v0.47.0 // indirect
)
