package auth

import "errors"

// Passwords hashes and verifies passwords with Argon2id. It satisfies the
// password interfaces of the users and session services.
type Passwords struct {
	params Argon2Params
	dummy  string // verified when no account matches, to equalize timing
}

// NewPasswords returns an Argon2id password helper using params.
func NewPasswords(params Argon2Params) (*Passwords, error) {
	dummy, err := HashPassword("open-kase timing equalizer", params)
	if err != nil {
		return nil, err
	}
	return &Passwords{params: params, dummy: dummy}, nil
}

// Hash returns a PHC-format Argon2id hash.
func (p *Passwords) Hash(password string) (string, error) { return HashPassword(password, p.params) }

// Verify returns nil when password matches hash. An empty hash (account
// without a password) still costs one hash computation and then fails.
func (p *Passwords) Verify(password, hash string) error {
	_, err := p.VerifyAndCheck(password, hash)
	return err
}

// VerifyAndCheck verifies password and reports whether the stored hash
// should be upgraded to the current parameters.
func (p *Passwords) VerifyAndCheck(password, hash string) (needsRehash bool, err error) {
	if hash == "" {
		_, _ = VerifyPassword(password, p.dummy, p.params)
		return false, ErrMismatch
	}
	needsRehash, err = VerifyPassword(password, hash, p.params)
	if err != nil && !errors.Is(err, ErrMismatch) {
		return false, ErrMismatch
	}
	return needsRehash, err
}
