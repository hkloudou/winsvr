package winsvr

import "encoding/xml"

// Manifest is a parsed Windows application manifest (an RT_MANIFEST resource).
//
// Note the struct tags: the requested execution level lives at the end of a
// nested element path and is read as an attribute of that element. The naive
// `xml:"requestedExecutionLevel>level,attr"` form does NOT compile at runtime —
// encoding/xml rejects a ">" element chain combined with the ",attr" flag —
// which is a common mistake when hand-rolling manifest parsing.
type Manifest struct {
	XMLName                 xml.Name `xml:"assembly"`
	RequestedExecutionLevel struct {
		// Level is one of "asInvoker", "highestAvailable" or
		// "requireAdministrator".
		Level string `xml:"level,attr"`
		// UIAccess is "true" or "false" (or empty if unspecified).
		UIAccess string `xml:"uiAccess,attr"`
	} `xml:"trustInfo>security>requestedPrivileges>requestedExecutionLevel"`
	SupportedOS []struct {
		ID string `xml:"Id,attr"`
	} `xml:"compatibility>application>supportedOS"`
	// Raw holds the undecoded manifest bytes.
	Raw []byte `xml:"-"`
}

// RequiresAdministrator reports whether the manifest asks Windows to run the
// executable elevated.
func (m *Manifest) RequiresAdministrator() bool {
	return m.RequestedExecutionLevel.Level == "requireAdministrator"
}

// ParseManifest decodes manifest XML.
func ParseManifest(data []byte) (*Manifest, error) {
	m := &Manifest{Raw: data}
	if err := xml.Unmarshal(data, m); err != nil {
		return nil, err
	}
	return m, nil
}
