package entities

// CheckGrant is the single rule every route that grants access applies,
// whether it is the /permissions command or the setup wizard: control is
// limited to the allowlisted domains. Read access can be granted on any entity.
func CheckGrant(id ID, access Access) error {
	if access == AccessControl && !id.Domain().Controllable() {
		return ErrNotControllable
	}
	return nil
}
