package controlapi

import (
	"bytes"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/go-playground/validator/v10"
	"github.com/petruekhin/vibe-firecracker/internal/vmbot"
	"github.com/petruekhin/vibe-firecracker/vmlib"
	"golang.org/x/crypto/ssh"
)

// FieldError is one machine-readable validation problem. Messages never echo
// submitted values (a key or token pasted in the wrong field must not come back).
type FieldError struct {
	Field   string `json:"field"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Absolute ceiling for a configured TTL (30 days): a guard against typos, not
// the product maximum, which is Policy.MaxTTLSeconds.
const hardMaxTTLSeconds = 30 * 24 * 3600

// MaxKeyBytes bounds a supplied public key line.
const MaxKeyBytes = 1024

var profileNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// Policy is the static public-contract policy derived from configuration and
// from which underlying features the integrator actually wired. A feature flag
// must only be set when the layer below really implements it: the API refuses
// the corresponding input (field code "unsupported") rather than accepting and
// ignoring it, and capabilities never advertise an unset feature.
type Policy struct {
	DefaultProfile    string `json:"default_profile" validate:"required,profile_name"`
	DefaultTTLSeconds int64  `json:"default_ttl_seconds" validate:"omitempty,gte=1"`
	MaxTTLSeconds     int64  `json:"max_ttl_seconds" validate:"omitempty,gte=1,lte=2592000"`
	// TTL: the owner persists a deadline and deletes the VM after it (stream C).
	TTL bool
	// SuppliedSSHKey: a supplied key is really installed before first boot (stream A).
	SuppliedSSHKey bool
	// GeneratedSSHKey: a generated key pair exists and Keys can return it.
	GeneratedSSHKey bool
}

// Validate checks static policy invariants with field-aware messages.
func (p Policy) Validate() error {
	if err := structValidator.Struct(p); err != nil {
		return fmtValidation("policy", err)
	}
	if (p.TTL || p.DefaultTTLSeconds != 0 || p.MaxTTLSeconds != 0) && (p.DefaultTTLSeconds < 1 || p.MaxTTLSeconds < p.DefaultTTLSeconds) {
		return errors.New("policy: default_ttl_seconds must be between 1 and max_ttl_seconds")
	}
	return nil
}

func fmtValidation(prefix string, err error) error {
	var ve validator.ValidationErrors
	if errors.As(err, &ve) && len(ve) > 0 {
		f := ve[0]
		return fmt.Errorf("%s: field %s failed %q", prefix, strings.ToLower(f.Field()), f.Tag())
	}
	return fmt.Errorf("%s: invalid", prefix)
}

// CreateInput is a fully validated and normalised Create request.
type CreateInput struct {
	Name, ImageID, Profile string
	// RequestedProfile preserves omission independently of the resolved default.
	RequestedProfile string
	// TTLSeconds is the effective TTL; 0 only when Policy.TTL is false.
	TTLSeconds int64
	// SSHPublicKey is the canonical "type base64" form (comment dropped).
	SSHPublicKey string
}

type createDTO struct {
	Name         string `json:"name" validate:"omitempty,max=64,vm_label"`
	ImageID      string `json:"image_id" validate:"required,max=64,resource_id"`
	Profile      string `json:"profile" validate:"omitempty,profile_name"`
	TTLSeconds   *int64 `json:"ttl_seconds" validate:"omitempty,gte=1"`
	SSHPublicKey string `json:"ssh_public_key" validate:"omitempty,max=1024,ssh_public_key,ssh_no_options"`
}

var structValidator = newValidator()

func newValidator() *validator.Validate {
	v := validator.New(validator.WithRequiredStructEnabled())
	v.RegisterTagNameFunc(func(f reflect.StructField) string {
		if n := strings.Split(f.Tag.Get("json"), ",")[0]; n != "" && n != "-" {
			return n
		}
		return strings.ToLower(f.Name)
	})
	must := func(tag string, fn validator.Func) {
		if err := v.RegisterValidation(tag, fn); err != nil {
			panic(err)
		}
	}
	must("profile_name", func(fl validator.FieldLevel) bool { return profileNamePattern.MatchString(fl.Field().String()) })
	must("vm_label", func(fl validator.FieldLevel) bool { return vmbot.ValidateLabel(fl.Field().String()) == nil })
	must("resource_id", func(fl validator.FieldLevel) bool { return vmlib.ValidateID("image", fl.Field().String()) == nil })
	must("ssh_public_key", func(fl validator.FieldLevel) bool {
		_, _, err := parsePublicKey(fl.Field().String())
		return err == nil
	})
	must("ssh_no_options", func(fl validator.FieldLevel) bool {
		_, opts, err := parsePublicKey(fl.Field().String())
		return err == nil && !opts
	})
	return v
}

var allowedKeyTypes = map[string]bool{
	ssh.KeyAlgoED25519: true, ssh.KeyAlgoRSA: true,
	ssh.KeyAlgoECDSA256: true, ssh.KeyAlgoECDSA384: true, ssh.KeyAlgoECDSA521: true,
	ssh.KeyAlgoSKED25519: true, ssh.KeyAlgoSKECDSA256: true,
}

// parsePublicKey really parses one OpenSSH authorized_keys-format public key
// with golang.org/x/crypto/ssh. It reports whether the line carried options
// (command=, from=, ...), which a user-supplied key must never contain. The
// returned canonical form drops the free-text comment.
func parsePublicKey(line string) (canonical string, hadOptions bool, err error) {
	if line == "" || len(line) > MaxKeyBytes || strings.ContainsAny(line, "\r\n\x00") {
		return "", false, errors.New("not a single-line key")
	}
	key, _, opts, rest, err := ssh.ParseAuthorizedKey([]byte(line))
	if err != nil {
		return "", false, err
	}
	if len(bytes.TrimSpace(rest)) != 0 {
		return "", false, errors.New("trailing data")
	}
	if !allowedKeyTypes[key.Type()] {
		return "", false, errors.New("unsupported key type")
	}
	if ck, ok := key.(ssh.CryptoPublicKey); ok {
		if rk, ok := ck.CryptoPublicKey().(*rsa.PublicKey); ok && rk.N.BitLen() < 2048 {
			return "", false, errors.New("RSA key too short")
		}
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))), len(opts) > 0, nil
}

var tagCodes = map[string]string{
	"required": "required", "max": "too_long", "gte": "out_of_range", "lte": "out_of_range",
	"vm_label": "invalid_format", "resource_id": "invalid_format", "profile_name": "invalid_format",
	"ssh_public_key": "invalid_format", "ssh_no_options": "options_not_allowed",
}

var tagMessages = map[string]string{
	"required":            "%s is required",
	"too_long":            "%s is too long",
	"out_of_range":        "%s is out of range",
	"invalid_format":      "%s has an invalid format",
	"options_not_allowed": "%s must be a plain public key without authorized_keys options",
}

func fe(field, code, msg string) FieldError {
	if m, ok := tagMessages[code]; ok && msg == "" {
		msg = fmt.Sprintf(m, field)
	}
	return FieldError{Field: field, Code: code, Message: msg}
}

// safeName renders a client-chosen field name for a message: bounded and free of
// control characters.
func safeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		if b.Len() >= 40 {
			break
		}
		if unicode.IsPrint(r) && r != '"' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

var createFields = []string{"name", "image_id", "profile", "ttl_seconds", "ssh_public_key"}

// ParseCreate strictly decodes and validates a Create body against the policy.
// Every problem found is reported (sorted by field), not only the first.
func ParseCreate(body []byte, p Policy) (CreateInput, []FieldError) {
	var errs []FieldError
	finish := func() (CreateInput, []FieldError) {
		sort.SliceStable(errs, func(i, j int) bool { return errs[i].Field < errs[j].Field })
		return CreateInput{}, errs
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	var top json.RawMessage
	if err := dec.Decode(&top); err != nil {
		errs = append(errs, fe("body", "invalid_json", "request body is not valid JSON"))
		return finish()
	}
	if _, err := dec.Token(); err != io.EOF {
		errs = append(errs, fe("body", "invalid_json", "request body must be exactly one JSON object"))
		return finish()
	}
	if t := bytes.TrimSpace(top); len(t) == 0 || t[0] != '{' {
		errs = append(errs, fe("body", "invalid_type", "request body must be a JSON object"))
		return finish()
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(top, &raw); err != nil {
		errs = append(errs, fe("body", "invalid_json", "request body is not valid JSON"))
		return finish()
	}
	known := map[string]bool{}
	for _, f := range createFields {
		known[f] = true
	}
	for k := range raw {
		switch {
		case k == "resources":
			errs = append(errs, fe("resources", "unknown_field", "resources is no longer accepted; select a profile with the profile field"))
		case !known[k]:
			n := safeName(k)
			errs = append(errs, fe(n, "unknown_field", fmt.Sprintf("unknown field %q", n)))
		}
	}
	var d createDTO
	typed := func(name string, dst any, expect string) {
		r, ok := raw[name]
		if !ok {
			return
		}
		if string(bytes.TrimSpace(r)) == "null" {
			errs = append(errs, fe(name, "invalid_type", fmt.Sprintf("%s must be %s", name, expect)))
			return
		}
		if err := json.Unmarshal(r, dst); err != nil {
			errs = append(errs, fe(name, "invalid_type", fmt.Sprintf("%s must be %s", name, expect)))
		}
	}
	typed("name", &d.Name, "a string")
	typed("image_id", &d.ImageID, "a string")
	typed("profile", &d.Profile, "a string")
	typed("ttl_seconds", &d.TTLSeconds, "an integer number of seconds")
	typed("ssh_public_key", &d.SSHPublicKey, "a string")
	bad := map[string]bool{}
	for _, e := range errs {
		bad[e.Field] = true
	}
	// Static validation, field by field, skipping fields that already failed.
	if err := structValidator.Struct(d); err != nil {
		var ve validator.ValidationErrors
		if errors.As(err, &ve) {
			for _, f := range ve {
				name := f.Field()
				if bad[name] {
					continue
				}
				code := tagCodes[f.Tag()]
				if code == "" {
					code = "invalid"
				}
				errs = append(errs, fe(name, code, ""))
				bad[name] = true
			}
		}
	}
	// Policy-dependent rules (the validator tags above hold only static rules).
	if d.TTLSeconds != nil && !bad["ttl_seconds"] {
		switch {
		case !p.TTL:
			errs = append(errs, fe("ttl_seconds", "unsupported", "ttl_seconds is not supported by this deployment"))
		case *d.TTLSeconds > p.MaxTTLSeconds:
			errs = append(errs, fe("ttl_seconds", "out_of_range", fmt.Sprintf("ttl_seconds must be between 1 and %d", p.MaxTTLSeconds)))
		}
	}
	if d.SSHPublicKey != "" && !p.SuppliedSSHKey {
		// Not format-checked: the feature is absent, whatever the content.
		kept := errs[:0]
		for _, e := range errs {
			if e.Field != "ssh_public_key" {
				kept = append(kept, e)
			}
		}
		errs = append(kept, fe("ssh_public_key", "unsupported", "supplying an SSH public key is not supported by this deployment"))
	}
	if len(errs) != 0 {
		return finish()
	}
	in := CreateInput{Name: d.Name, ImageID: d.ImageID, Profile: d.Profile, RequestedProfile: d.Profile}
	if in.Profile == "" {
		in.Profile = p.DefaultProfile
	}
	if p.TTL {
		if d.TTLSeconds != nil {
			in.TTLSeconds = *d.TTLSeconds
		}
	}
	if d.SSHPublicKey != "" {
		in.SSHPublicKey, _, _ = parsePublicKey(d.SSHPublicKey)
	}
	return in, nil
}
