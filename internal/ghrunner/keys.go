package ghrunner

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/xml"
	"math/big"
)

// keySize matches the real runner: RSA-2048.
const keySize = 2048

// generateKey creates the runner's RSA keypair used for OAuth client-assertion
// signing and session-key decryption.
func generateKey() (*rsa.PrivateKey, error) {
	return rsa.GenerateKey(rand.Reader, keySize)
}

// publicParams returns the modulus and exponent of a public key as standard
// (not URL-safe) base64, matching .NET's RSAParameters serialization used by the
// classic agent registration payload.
func publicParams(pub *rsa.PublicKey) (modulus, exponent string) {
	modulus = base64.StdEncoding.EncodeToString(pub.N.Bytes())
	e := big.NewInt(int64(pub.E))
	exponent = base64.StdEncoding.EncodeToString(e.Bytes())
	return modulus, exponent
}

type rsaKeyValue struct {
	XMLName  xml.Name `xml:"RSAKeyValue"`
	Modulus  string   `xml:"Modulus"`
	Exponent string   `xml:"Exponent"`
}

// publicKeyXML renders the public key as a .NET RSA.ToXmlString() string, which
// the v2/dotcom registration endpoint expects for the "public_key" field.
func publicKeyXML(pub *rsa.PublicKey) (string, error) {
	mod, exp := publicParams(pub)
	out, err := xml.Marshal(rsaKeyValue{Modulus: mod, Exponent: exp})
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// rsaParams is the persisted private-key material (.credentials_rsaparams),
// stored as base64 big-endian components so we can reconstruct the key on
// subsequent runs without re-registering.
type rsaParams struct {
	D        string `json:"d"`
	DP       string `json:"dp"`
	DQ       string `json:"dq"`
	Exponent string `json:"exponent"`
	InverseQ string `json:"inverseQ"`
	Modulus  string `json:"modulus"`
	P        string `json:"p"`
	Q        string `json:"q"`
}

func exportParams(key *rsa.PrivateKey) rsaParams {
	key.Precompute()
	b64 := func(i *big.Int) string { return base64.StdEncoding.EncodeToString(i.Bytes()) }
	return rsaParams{
		D:        b64(key.D),
		DP:       b64(key.Precomputed.Dp),
		DQ:       b64(key.Precomputed.Dq),
		Exponent: base64.StdEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		InverseQ: b64(key.Precomputed.Qinv),
		Modulus:  b64(key.N),
		P:        b64(key.Primes[0]),
		Q:        b64(key.Primes[1]),
	}
}

func (p rsaParams) toKey() (*rsa.PrivateKey, error) {
	dec := func(s string) (*big.Int, error) {
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return nil, err
		}
		return new(big.Int).SetBytes(b), nil
	}
	n, err := dec(p.Modulus)
	if err != nil {
		return nil, err
	}
	e, err := dec(p.Exponent)
	if err != nil {
		return nil, err
	}
	d, err := dec(p.D)
	if err != nil {
		return nil, err
	}
	pp, err := dec(p.P)
	if err != nil {
		return nil, err
	}
	q, err := dec(p.Q)
	if err != nil {
		return nil, err
	}
	key := &rsa.PrivateKey{
		PublicKey: rsa.PublicKey{N: n, E: int(e.Int64())},
		D:         d,
		Primes:    []*big.Int{pp, q},
	}
	key.Precompute()
	return key, key.Validate()
}
