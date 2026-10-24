package domain

// CanonicalRole reads an empty role as RoleBoth. Every role comparison goes
// through it -- the analyzers, aggregation and the composite -- so a variant
// with no role label is keyed the same way everywhere.
func CanonicalRole(role string) string {
	if role == "" {
		return RoleBoth
	}
	return role
}

// HasPrefill reports whether rolesByVariant (variant name to role) names a
// prefill variant: the model is split into prefill and decode, and its decode
// replicas' waiting counts hold requests waiting for their KV from prefill.
// Whether the prefill variant has a Ready replica does not matter; the split
// is the model's shape.
func HasPrefill(rolesByVariant map[string]string) bool {
	for _, role := range rolesByVariant {
		if CanonicalRole(role) == RolePrefill {
			return true
		}
	}
	return false
}

// RoleOfVC is vc's role, canonicalized (CanonicalRole).
func RoleOfVC(vc VariantCapacity) string {
	return CanonicalRole(vc.Role)
}
