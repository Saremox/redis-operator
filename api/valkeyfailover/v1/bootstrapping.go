package v1

// Bootstrapping returns true when a BootstrapNode is provided to the ValkeyFailover spec. Otherwise, it returns false.
func (r *ValkeyFailover) Bootstrapping() bool {
	return r.Spec.BootstrapNode != nil
}

// SentinelsAllowed returns true if not Bootstrapping orif BootstrapNode settings allow sentinels to exist
func (r *ValkeyFailover) SentinelsAllowed() bool {
	bootstrapping := r.Bootstrapping()
	return !bootstrapping || (bootstrapping && r.Spec.BootstrapNode.AllowSentinels)
}
