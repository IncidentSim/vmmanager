package vmbot

// ReplayControl is a read-only association lookup. It deliberately cannot
// admit requests, resolve profiles, or invoke effects. Authorization precedes
// even the lookup; a foreign owner's key is indistinguishable from absence.
func (c *Controller) ReplayControl(u int64, key string, digests []string) (ControlOperation, bool, error) {
	if !c.ControlAllowed(u) {
		return ControlOperation{}, false, ErrControlForbidden
	}
	for _, o := range c.st.Snapshot().Operations {
		if o.OwnerUserID != u || o.Key != key {
			continue
		}
		for _, digest := range digests {
			if o.Digest == digest && o.Kind == "create" {
				return o, true, nil
			}
		}
		return ControlOperation{}, true, ErrControlConflict
	}
	return ControlOperation{}, false, nil
}
