package image

// Media types the engine understands. The OCI ones come from the image
// specification, the wasm ones from the CNCF TAG-Runtime Wasm OCI artifact
// layout, and the Docker ones are the schema 2 types that registries still
// serve for images built by Docker.
const (
	MediaTypeOCIManifest = "application/vnd.oci.image.manifest.v1+json"
	MediaTypeOCIIndex    = "application/vnd.oci.image.index.v1+json"
	MediaTypeOCIConfig   = "application/vnd.oci.image.config.v1+json"
	MediaTypeOCILayer    = "application/vnd.oci.image.layer.v1.tar"
	MediaTypeOCILayerGz  = "application/vnd.oci.image.layer.v1.tar+gzip"
	MediaTypeOCILayerZst = "application/vnd.oci.image.layer.v1.tar+zstd"
	// MediaTypePkgxLayerZst is a pkgx bottle's layer (go-pkgx/packages).
	MediaTypePkgxLayerZst = "application/vnd.pkgx.bottle.layer.v1.tar+zstd"

	MediaTypeDockerManifest = "application/vnd.docker.distribution.manifest.v2+json"
	MediaTypeDockerList     = "application/vnd.docker.distribution.manifest.list.v2+json"
	MediaTypeDockerConfig   = "application/vnd.docker.container.image.v1+json"
	MediaTypeDockerLayerGz  = "application/vnd.docker.image.rootfs.diff.tar.gzip"

	MediaTypeWasmConfig = "application/vnd.wasm.config.v0+json"
	MediaTypeWasmLayer  = "application/wasm"
)

// Annotation keys used by the engine.
const (
	AnnotationRefName = "org.opencontainers.image.ref.name"
	AnnotationTitle   = "org.opencontainers.image.title"
)

// Platform is the platform of a manifest in an index.
type Platform struct {
	Architecture string   `json:"architecture"`
	OS           string   `json:"os"`
	OSVersion    string   `json:"os.version,omitempty"`
	OSFeatures   []string `json:"os.features,omitempty"`
	Variant      string   `json:"variant,omitempty"`
}

// String renders "os/arch[/variant]".
func (p Platform) String() string {
	s := p.OS + "/" + p.Architecture
	if p.Variant != "" {
		s += "/" + p.Variant
	}
	return s
}

// WasmPlatform is the only platform the engine runs.
var WasmPlatform = Platform{OS: "wasip1", Architecture: "wasm"}

// Descriptor points at content by digest and size.
type Descriptor struct {
	MediaType    string            `json:"mediaType"`
	Digest       Digest            `json:"digest"`
	Size         int64             `json:"size"`
	URLs         []string          `json:"urls,omitempty"`
	Annotations  map[string]string `json:"annotations,omitempty"`
	Data         []byte            `json:"data,omitempty"`
	Platform     *Platform         `json:"platform,omitempty"`
	ArtifactType string            `json:"artifactType,omitempty"`
}

// Manifest is an OCI image manifest (or a Docker schema 2 manifest, which
// has the same shape).
type Manifest struct {
	SchemaVersion int               `json:"schemaVersion"`
	MediaType     string            `json:"mediaType,omitempty"`
	ArtifactType  string            `json:"artifactType,omitempty"`
	Config        Descriptor        `json:"config"`
	Layers        []Descriptor      `json:"layers"`
	Subject       *Descriptor       `json:"subject,omitempty"`
	Annotations   map[string]string `json:"annotations,omitempty"`
}

// Index is an OCI image index (or a Docker manifest list).
type Index struct {
	SchemaVersion int               `json:"schemaVersion"`
	MediaType     string            `json:"mediaType,omitempty"`
	ArtifactType  string            `json:"artifactType,omitempty"`
	Manifests     []Descriptor      `json:"manifests"`
	Subject       *Descriptor       `json:"subject,omitempty"`
	Annotations   map[string]string `json:"annotations,omitempty"`
}

// ExecConfig is the "config" object of an image config: how to run it.
type ExecConfig struct {
	User         string              `json:"User,omitempty"`
	ExposedPorts map[string]struct{} `json:"ExposedPorts,omitempty"`
	Env          []string            `json:"Env,omitempty"`
	Entrypoint   []string            `json:"Entrypoint,omitempty"`
	Cmd          []string            `json:"Cmd,omitempty"`
	Volumes      map[string]struct{} `json:"Volumes,omitempty"`
	WorkingDir   string              `json:"WorkingDir,omitempty"`
	Labels       map[string]string   `json:"Labels,omitempty"`
	StopSignal   string              `json:"StopSignal,omitempty"`
}

// RootFS lists the layers' uncompressed digests.
type RootFS struct {
	Type    string   `json:"type"`
	DiffIDs []string `json:"diff_ids"`
}

// Config is an OCI image config, or a CNCF wasm artifact config: the two
// share created/author/architecture/os, and each ignores the other's extra
// fields.
type Config struct {
	Created      string     `json:"created,omitempty"`
	Author       string     `json:"author,omitempty"`
	Architecture string     `json:"architecture"`
	OS           string     `json:"os"`
	Variant      string     `json:"variant,omitempty"`
	Config       ExecConfig `json:"config"`
	RootFS       RootFS     `json:"rootfs"`

	// LayerDigests is the wasm artifact's list of layer digests.
	LayerDigests []string `json:"layerDigests,omitempty"`
}

// Image is a pulled image as the store knows it: one platform manifest,
// its config, and the names that refer to it.
type Image struct {
	ManifestDigest Digest
	Manifest       *Manifest
	ConfigDigest   Digest
	Config         *Config
	// Size is the total size of the config and layer blobs.
	Size int64
	// Wasm reports a CNCF wasm artifact (single application/wasm layer).
	Wasm bool
}

// ID is the image ID in Docker's sense: the config digest.
func (i *Image) ID() Digest { return i.ConfigDigest }
