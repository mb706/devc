package fleet

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/mb706/devc/internal/archive"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/secrets"
)

// SecretSet is the plaintext of a sealed secrets bundle: a realm's stored
// secrets (values in base64).
type SecretSet struct {
	V       int               `json:"v"`
	Realm   string            `json:"realm"`
	Machine string            `json:"machine"`
	Created time.Time         `json:"created"`
	Secrets map[string]string `json:"secrets"`
}

// maxSecrets bounds the secrets in one bundle.
const maxSecrets = 1000

// SealSecrets reads every secret of realm from store and seals them with
// passphrase. It returns the bundle and the number of secrets.
func SealSecrets(store secrets.Store, realm, machine string, passphrase []byte) (*archive.Sealed, int, error) {
	list, err := store.List(realm)
	if err != nil {
		return nil, 0, err
	}
	if len(list) > maxSecrets {
		return nil, 0, fmt.Errorf("realm %s has more than %d secrets", realm, maxSecrets)
	}
	set := SecretSet{V: 1, Realm: realm, Machine: machine, Created: time.Now().UTC(), Secrets: map[string]string{}}
	for _, n := range list {
		v, err := store.Get(realm, n)
		if err != nil {
			return nil, 0, err
		}
		set.Secrets[n] = base64.StdEncoding.EncodeToString(v)
	}
	plain, err := json.Marshal(set)
	if err != nil {
		return nil, 0, err
	}
	s, err := archive.Seal(passphrase, realm, plain)
	clear(plain)
	if err != nil {
		return nil, 0, err
	}
	return s, len(list), nil
}

// OpenSecrets decrypts a bundle of realm and returns its secrets, every
// name and value checked.
func OpenSecrets(s *archive.Sealed, realm string, passphrase []byte) (map[string][]byte, *SecretSet, error) {
	plain, err := s.Open(passphrase, realm)
	if err != nil {
		return nil, nil, err
	}
	defer clear(plain)
	dec := json.NewDecoder(bytes.NewReader(plain))
	dec.DisallowUnknownFields()
	var set SecretSet
	if err := dec.Decode(&set); err != nil || set.V != 1 || set.Realm != realm {
		return nil, nil, errors.New("the decrypted bundle is not a secrets bundle of this realm")
	}
	if len(set.Secrets) > maxSecrets {
		return nil, nil, errors.New("the bundle holds too many secrets")
	}
	out := map[string][]byte{}
	for n, v := range set.Secrets {
		if !names.ValidSecret(n) {
			return nil, nil, fmt.Errorf("the bundle holds an invalid secret name %q", n)
		}
		b, err := base64.StdEncoding.DecodeString(v)
		if err != nil || len(b) == 0 || len(b) > secrets.MaxSize {
			return nil, nil, fmt.Errorf("the bundle holds an invalid value for %s", n)
		}
		out[n] = b
	}
	set.Secrets = nil
	return out, &set, nil
}
