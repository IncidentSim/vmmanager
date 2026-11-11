package controlapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"

	"github.com/petruekhin/vibe-firecracker/internal/vmbot"
)

// createReplayDigest decodes intent only, never resolves defaults or catalogues.
// The retired DTO is deliberately local: it can identify an existing operation,
// but cannot be passed to admission. Keep its field order/tags identical to the
// pre-profile ControlRequest, whose marshalled value was the persisted digest.
func createReplayDigests(body []byte) []string {
	var digests []string
	var old struct {
		Kind      string `json:"kind"`
		VMID      string `json:"vm_id,omitempty"`
		ImageID   string `json:"image_id,omitempty"`
		Name      string `json:"name,omitempty"`
		Resources *struct {
			VCPU      uint32 `json:"vcpu"`
			MemoryMiB uint32 `json:"memory_mib"`
		} `json:"resources,omitempty"`
		SSHPublicKey string `json:"ssh_public_key,omitempty"`
	}
	// Only the original HTTP fields, not internal Kind/VMID, are accepted.
	var dto struct {
		Name      string `json:"name"`
		ImageID   string `json:"image_id"`
		Resources *struct {
			VCPU      uint32 `json:"vcpu"`
			MemoryMiB uint32 `json:"memory_mib"`
		} `json:"resources"`
		SSHPublicKey string `json:"ssh_public_key"`
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&dto); err == nil && dec.Decode(new(any)) == io.EOF && bytes.HasPrefix(bytes.TrimSpace(body), []byte("{")) {
		old.Kind, old.Name, old.ImageID, old.Resources, old.SSHPublicKey = "create", dto.Name, dto.ImageID, dto.Resources, dto.SSHPublicKey
		b, _ := json.Marshal(old)
		sum := sha256.Sum256(b)
		digests = append(digests, hex.EncodeToString(sum[:]))
	}
	// Current intent uses canonical keys but no mutable deployment validation.
	in, fields := ParseCreate(body, Policy{TTL: true, MaxTTLSeconds: hardMaxTTLSeconds, SuppliedSSHKey: true})
	if len(fields) != 0 {
		return digests
	}
	req := vmbot.ControlRequest{Kind: "create", IntentVersion: 1, Name: in.Name, ImageID: in.ImageID, Profile: in.RequestedProfile, TTLSeconds: in.TTLSeconds, SSHPublicKey: in.SSHPublicKey}
	b, _ := json.Marshal(req)
	sum := sha256.Sum256(b)
	return append(digests, hex.EncodeToString(sum[:]))
}
