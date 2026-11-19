package sysprompt

// mustParse parses the embedded template. A broken embedded file is a
// build defect, so it panics; the golden test catches it first.
func mustParse(src string) *Template {
	t, err := Parse(src)
	mustOK(err)
	return t
}

func mustOK(err error) {
	if err != nil {
		panic(err)
	}
}
