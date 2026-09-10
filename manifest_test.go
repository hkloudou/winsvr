package winsvr

import "testing"

const sampleManifest = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<assembly xmlns="urn:schemas-microsoft-com:asm.v1" manifestVersion="1.0">
  <trustInfo xmlns="urn:schemas-microsoft-com:asm.v3">
    <security><requestedPrivileges>
      <requestedExecutionLevel level="requireAdministrator" uiAccess="false"/>
    </requestedPrivileges></security>
  </trustInfo>
  <compatibility xmlns="urn:schemas-microsoft-com:compatibility.v1"><application>
    <supportedOS Id="{8e0f7a12-bfb3-4fe8-b9a5-48fd50a15a9a}"/>
  </application></compatibility>
</assembly>`

func TestParseManifest(t *testing.T) {
	m, err := ParseManifest([]byte(sampleManifest))
	if err != nil {
		t.Fatal(err)
	}
	if got := m.RequestedExecutionLevel.Level; got != "requireAdministrator" {
		t.Fatalf("level = %q", got)
	}
	if !m.RequiresAdministrator() {
		t.Fatal("RequiresAdministrator = false")
	}
	if len(m.SupportedOS) != 1 {
		t.Fatalf("supportedOS = %d, want 1", len(m.SupportedOS))
	}
}
