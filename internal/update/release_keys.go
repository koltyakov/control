package update

// Reviewed public release pins. An empty keyring rejects every public release.
// Never add test keys, private keys, runtime overrides, or
// keys fetched from release assets. See docs/updates.md#release-signing-keys.
//
// Each entry is an exact repository mapped to key ID and base64 raw 32-byte key.
// Multiple entries support an explicitly staged key rotation.
var releaseKeys = ReleaseKeyring{
	"koltyakov/control": {
		"release-2026-11-17": "5qmDhioRpJ9KhklhqNgjWuxv1wzplaHPw5g+aJI7uiA=",
	},
}
