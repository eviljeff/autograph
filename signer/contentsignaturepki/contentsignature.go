package contentsignaturepki // import "go.mozilla.org/autograph/signer/contentsignaturepki"

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/asn1"
	"fmt"
	"hash"
	"io"
	"time"

	"go.mozilla.org/autograph/database"
	"go.mozilla.org/autograph/signer"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"go.mozilla.org/mozlogrus"
)

func init() {
	// initialize the logger
	mozlogrus.Enable("autograph")
}

const (
	// Type of this signer is 'contentsignaturepki'
	Type = "contentsignaturepki"

	// P256ECDSA defines an ecdsa content signature on the P-256 curve
	P256ECDSA = "p256ecdsa"

	// P256ECDSABYTESIZE defines the bytes length of a P256ECDSA signature
	P256ECDSABYTESIZE = 64

	// P384ECDSA defines an ecdsa content signature on the P-384 curve
	P384ECDSA = "p384ecdsa"

	// P384ECDSABYTESIZE defines the bytes length of a P384ECDSA signature
	P384ECDSABYTESIZE = 96

	// SignaturePrefix is a string preprended to data prior to signing
	SignaturePrefix = "Content-Signature:\x00"

	// CSNameSpace is a string that contains the namespace on which
	// content signature certificates are issued
	CSNameSpace = ".content-signature.mozilla.org"
)

// ContentSigner implements an issuer of content signatures
type ContentSigner struct {
	signer.Configuration
	issuerPriv, eePriv  crypto.PrivateKey
	issuerPub, eePub    crypto.PublicKey
	eeLabel             string
	rand                io.Reader
	validity            time.Duration
	clockSkewTolerance  time.Duration
	chainUploadLocation string
	chain               string
	caCert              string
	db                  *database.Handler
}

// New initializes a ContentSigner using a signer configuration
func New(conf signer.Configuration) (s *ContentSigner, err error) {
	s = new(ContentSigner)
	s.ID = conf.ID
	s.Type = conf.Type
	s.PrivateKey = conf.PrivateKey
	s.PublicKey = conf.PublicKey
	s.X5U = conf.X5U
	s.validity = conf.Validity
	s.clockSkewTolerance = conf.ClockSkewTolerance
	s.chainUploadLocation = conf.ChainUploadLocation
	s.caCert = conf.CaCert
	s.db = conf.DB

	if conf.Type != Type {
		return nil, errors.Errorf("contentsignaturepki: invalid type %q, must be %q", conf.Type, Type)
	}
	if conf.ID == "" {
		return nil, errors.New("contentsignaturepki: missing signer ID in signer configuration")
	}
	if conf.PrivateKey == "" {
		return nil, errors.New("contentsignaturepki: missing private key in signer configuration")
	}
	s.issuerPriv, s.issuerPub, s.rand, _, err = conf.GetKeysAndRand()
	if err != nil {
		return nil, errors.Wrapf(err, "contentsignaturepki: failed to get keys and rand for signer %q", conf.ID)
	}
	// if validity is undef, default to 30 days
	if s.validity == 0 {
		log.Printf("contentsignaturepki: no validity configured for signer %s, defaulting to 30 days", s.ID)
		s.validity = 720 * time.Hour
	}

	switch s.issuerPub.(type) {
	case *ecdsa.PublicKey:
	default:
		return nil, errors.New("contentsignaturepki: invalid public key type for issuer, must be ecdsa")
	}
	s.Mode = s.getModeFromCurve()

	// the end-entity key is not stored in configuration but may already
	// exist in an hsm, if present. Try to retrieve it, or make a new one.
	var tx *database.Transaction
	if s.db != nil {
		tx, err = s.db.BeginEndEntityOperations()
		if err != nil {
			return nil, errors.Wrap(err, "contentsignaturepki: failed to begin end-entity db operations")
		}
	}
	err = s.findAndSetEE(conf, tx)
	if err != nil {
		if err == database.ErrNoSuitableEEFound {
			log.Printf("contentsignaturepki: making new end-entity for signer %q", s.ID)
			// create a label and generate the key
			s.eeLabel = fmt.Sprintf("%s-%s", s.ID, time.Now().UTC().Format("20060102150405"))
			s.eePriv, s.eePub, err = conf.MakeKey(s.issuerPub, s.eeLabel)
			if err != nil {
				err = errors.Wrap(err, "failed to generate key for end entity")
				return
			}
			// make the certificate and upload the chain
			err = s.makeAndUploadChain()
			if err != nil {
				return nil, errors.Wrap(err, "contentsignaturepki: failed to make chain and x5u for end-entity")
			}
			if tx != nil {
				// insert it in database
				hsmHandle := signer.GetPrivKeyHandle(s.eePriv)
				err = tx.InsertEE(s.X5U, s.eeLabel, s.ID, hsmHandle)
				if err != nil {
					return nil, errors.Wrap(err, "contentsignaturepki: failed to insert new EE into database")
				}
			}
		} else {
			return nil, errors.Wrap(err, "contentsignaturepki: failed to find suitable end-entity")
		}
	}

	// check if we already have a valid x5u, and if not make a new chain,
	// upload it and re-verify
	_, err = GetX5U(s.X5U)
	if err != nil {
		return nil, errors.Wrap(err, "contentsignaturepki: failed to verify x5u")
	}
	if tx != nil {
		err = tx.End()
		if err != nil {
			return nil, errors.Wrap(err, "contentsignaturepki: failed to commit end-entity operations in database")
		}
	}
	return
}

// Config returns the configuration of the current signer
func (s *ContentSigner) Config() signer.Configuration {
	return signer.Configuration{
		ID:                  s.ID,
		Type:                s.Type,
		Mode:                s.Mode,
		PrivateKey:          s.PrivateKey,
		PublicKey:           s.PublicKey,
		X5U:                 s.X5U,
		Validity:            s.validity,
		ClockSkewTolerance:  s.clockSkewTolerance,
		ChainUploadLocation: s.chainUploadLocation,
		CaCert:              s.caCert,
	}
}

// SignData takes input data, templates it, hashes it and signs it.
// The returned signature is of type ContentSignature and ready to be Marshalled.
func (s *ContentSigner) SignData(input []byte, options interface{}) (signer.Signature, error) {
	if len(input) < 10 {
		return nil, errors.Errorf("contentsignaturepki: refusing to sign input data shorter than 10 bytes")
	}
	alg, hash := MakeTemplatedHash(input, s.Mode)
	sig, err := s.SignHash(hash, options)
	sig.(*ContentSignature).storeHashName(alg)
	return sig, err
}

// MakeTemplatedHash returns the templated sha384 of the input data. The template adds
// the string "Content-Signature:\x00" before the input data prior to
// calculating the sha384.
//
// The name of the hash function is returned, followed by the hash bytes
func MakeTemplatedHash(data []byte, curvename string) (alg string, out []byte) {
	templated := make([]byte, len(SignaturePrefix)+len(data))
	copy(templated[:len(SignaturePrefix)], []byte(SignaturePrefix))
	copy(templated[len(SignaturePrefix):], data)
	var md hash.Hash
	switch curvename {
	case P384ECDSA:
		md = sha512.New384()
		alg = "sha384"
	default:
		md = sha256.New()
		alg = "sha256"
	}
	md.Write(templated)
	return alg, md.Sum(nil)
}

// SignHash takes an input hash and returns a signature. It assumes the input data
// has already been hashed with something like sha384
func (s *ContentSigner) SignHash(input []byte, options interface{}) (signer.Signature, error) {
	if len(input) != 32 && len(input) != 48 && len(input) != 64 {
		return nil, errors.Errorf("contentsignaturepki: refusing to sign input hash. length %d, expected 32, 48 or 64", len(input))
	}
	var err error
	csig := new(ContentSignature)
	csig = &ContentSignature{
		Len:  getSignatureLen(s.Mode),
		Mode: s.Mode,
		X5U:  s.X5U,
		ID:   s.ID,
	}

	asn1Sig, err := s.eePriv.(crypto.Signer).Sign(rand.Reader, input, nil)
	if err != nil {
		return nil, errors.Wrap(err, "contentsignaturepki: failed to sign hash")
	}
	var ecdsaSig ecdsaAsn1Signature
	_, err = asn1.Unmarshal(asn1Sig, &ecdsaSig)
	if err != nil {
		return nil, errors.Wrap(err, "contentsignaturepki: failed to parse signature")
	}
	csig.R = ecdsaSig.R
	csig.S = ecdsaSig.S
	csig.Finished = true
	return csig, nil
}

// getSignatureLen returns the size of an ECDSA signature issued by the signer,
// or -1 if the mode is unknown
//
// The signature length is double the size size of the curve field, in bytes
// (each R and S value is equal to the size of the curve field).
// If the curve field it not a multiple of 8, round to the upper multiple of 8.
func getSignatureLen(mode string) int {
	switch mode {
	case P256ECDSA:
		return P256ECDSABYTESIZE
	case P384ECDSA:
		return P384ECDSABYTESIZE
	}
	return -1
}

// getSignatureHash returns the name of the hash function used by a given mode,
// or an empty string if the mode is unknown
func getSignatureHash(mode string) string {
	switch mode {
	case P256ECDSA:
		return "sha256"
	case P384ECDSA:
		return "sha384"
	}
	return ""
}

// getModeFromCurve returns a content signature algorithm name, or an empty string if the mode is unknown
func (s *ContentSigner) getModeFromCurve() string {
	switch s.issuerPub.(*ecdsa.PublicKey).Params().Name {
	case "P-256":
		return P256ECDSA
	case "P-384":
		return P384ECDSA
	default:
		return ""
	}
}

// GetDefaultOptions returns nil because this signer has no option
func (s *ContentSigner) GetDefaultOptions() interface{} {
	return nil
}

// Verify takes the location of a cert chain (x5u), a signature in its
// raw base64_url format and input data. It then performs a verification
// of the signature on the input data using the end-entity certificate
// of the chain, and returns an error if it fails, or nil on success.
func Verify(x5u, signature string, input []byte) error {
	certs, err := GetX5U(x5u)
	if err != nil {
		return err
	}
	// Get the public key from the end-entity
	if len(certs) < 1 {
		return fmt.Errorf("no certificate found in x5u")
	}
	key := certs[0].PublicKey.(*ecdsa.PublicKey)
	// parse the json signature
	sig, err := Unmarshal(signature)
	if err != nil {
		return err
	}
	// make a templated hash
	if !sig.VerifyData(input, key) {
		return fmt.Errorf("ecdsa signature verification failed")
	}
	return nil
}