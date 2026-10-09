package ports

type AdapterManifest struct {
	Slug         string
	Name         string
	Version      string
	Protocols    []string
	Capabilities []string
}

type AdapterRegistration struct {
	Manifest AdapterManifest
	Adapter  Adapter
}
