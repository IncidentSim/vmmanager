package controlapi

// PublicPolicy supplies configured defaults, but intentionally enables no
// provisioning/lifetime feature. The integrator sets flags only after wiring
// the actual durable owner/access provider, then passes it to Server.Configure.
func (c Config) PublicPolicy() Policy {
	return Policy{DefaultProfile: c.DefaultProfile, DefaultTTLSeconds: c.DefaultTTLSeconds, MaxTTLSeconds: c.MaxTTLSeconds}
}
