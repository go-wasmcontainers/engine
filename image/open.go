package image

// OpenSource turns a pull reference into a Source and the local name the
// pulled image is stored under. "oci:<dir>[:<ref.name>]" opens an OCI image
// layout; anything else is a registry reference.
func OpenSource(s string, opt RegistryOptions, lim Limits) (Source, string, error) {
	if IsLayoutRef(s) {
		lr, err := ParseLayoutRef(s)
		if err != nil {
			return nil, "", err
		}
		name, err := lr.LocalName()
		if err != nil {
			return nil, "", err
		}
		src, err := OpenLayoutDir(lr, lim)
		if err != nil {
			return nil, "", err
		}
		return src, name, nil
	}
	ref, err := ParseReference(s)
	if err != nil {
		return nil, "", err
	}
	return NewRegistrySource(ref, opt, lim), ref.String(), nil
}
