package main

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"path"

	"github.com/google/go-tpm/tpm2"
	"github.com/google/go-tpm/tpm2/transport"
	"golang.org/x/crypto/cryptobyte"
	"golang.org/x/crypto/cryptobyte/asn1"
	"golang.org/x/crypto/ssh"
)

// We need to know if the TPM handle has a pin set
type PINStatus uint8

const (
	NoPIN PINStatus = iota
	HasPIN
)

func (p PINStatus) String() string {
	switch p {
	case NoPIN:
		return "NoPIN"
	case HasPIN:
		return "HasPIN"
	}
	return "Not a PINStatus"
}

type Key struct {
	Version uint8
	PIN     PINStatus
	Private tpm2.TPM2BPrivate
	Public  tpm2.TPM2BPublic
}

func (k *Key) PublicKey() (*ecdsa.PublicKey, error) {
	c := tpm2.BytesAs2B[tpm2.TPMTPublic](k.Public.Bytes())
	pub, err := c.Contents()
	if err != nil {
		return nil, err
	}
	ecc, err := pub.Unique.ECC()
	if err != nil {
		return nil, err
	}

	ecdsaKey := &ecdsa.PublicKey{Curve: elliptic.P256(),
		X: big.NewInt(0).SetBytes(ecc.X.Buffer),
		Y: big.NewInt(0).SetBytes(ecc.Y.Buffer),
	}

	return ecdsaKey, nil
}

func (k *Key) SSHPublicKey() (ssh.PublicKey, error) {
	pubkey, err := k.PublicKey()
	if err != nil {
		return nil, err
	}
	return ssh.NewPublicKey(pubkey)
}

func UnmarshalKey(b []byte) (*Key, error) {
	var key Key

	r := bytes.NewBuffer(b)

	for _, k := range []interface{}{
		&key.Version,
		&key.PIN,
	} {
		if err := binary.Read(r, binary.BigEndian, k); err != nil {
			return nil, err
		}
	}

	public, err := tpm2.Unmarshal[tpm2.TPM2BPublic](r.Bytes())
	if err != nil {
		return nil, err
	}

	private, err := tpm2.Unmarshal[tpm2.TPM2BPrivate](r.Bytes()[len(public.Bytes())+2:])
	if err != nil {
		return nil, err
	}

	key.Public = *public
	key.Private = *private

	return &key, err
}

func MarshalKey(k *Key) []byte {
	var b bytes.Buffer
	binary.Write(&b, binary.BigEndian, k.Version)
	binary.Write(&b, binary.BigEndian, k.PIN)

	var pub []byte
	pub = append(pub, tpm2.Marshal(k.Public)...)
	pub = append(pub, tpm2.Marshal(k.Private)...)
	b.Write(pub)

	return b.Bytes()
}

// Creates a Storage Key, or return the loaded storage key
func CreateSRK(tpm transport.TPMCloser) (*tpm2.AuthHandle, *tpm2.TPMTPublic, error) {
	srk := tpm2.CreatePrimary{
		PrimaryHandle: tpm2.TPMRHOwner,
		InSensitive: tpm2.TPM2BSensitiveCreate{
			Sensitive: &tpm2.TPMSSensitiveCreate{
				UserAuth: tpm2.TPM2BAuth{
					Buffer: []byte(nil),
				},
			},
		},
		InPublic: tpm2.New2B(tpm2.ECCSRKTemplate),
	}

	var rsp *tpm2.CreatePrimaryResponse
	rsp, err := srk.Execute(tpm)
	if err != nil {
		return nil, nil, fmt.Errorf("failed creating primary key: %v", err)
	}

	srkPublic, err := rsp.OutPublic.Contents()
	if err != nil {
		return nil, nil, fmt.Errorf("failed getting srk public content: %v", err)
	}

	return &tpm2.AuthHandle{
		Handle: rsp.ObjectHandle,
		Name:   rsp.Name,
		Auth:   tpm2.PasswordAuth(nil),
	}, srkPublic, nil
}

type TPMSigner struct {
	key *Key
	tpm func() transport.TPMCloser
	pin func(*Key) ([]byte, error)
}

var _ crypto.Signer = &TPMSigner{}

func NewTPMSigner(k *Key, tpm func() transport.TPMCloser, pin func(*Key) ([]byte, error)) *TPMSigner {
	return &TPMSigner{
		key: k,
		tpm: tpm,
		pin: pin,
	}
}

func (t *TPMSigner) getTPM() transport.TPMCloser {
	return t.tpm()
}

func (t *TPMSigner) Public() crypto.PublicKey {
	pk, err := t.key.PublicKey()
	// This shouldn't happen!
	if err != nil {
		panic(err)
	}
	return pk
}

// from crypto/ecdsa
func addASN1IntBytes(b *cryptobyte.Builder, bytes []byte) {
	for len(bytes) > 0 && bytes[0] == 0 {
		bytes = bytes[1:]
	}
	if len(bytes) == 0 {
		b.SetError(errors.New("invalid integer"))
		return
	}
	b.AddASN1(asn1.INTEGER, func(c *cryptobyte.Builder) {
		if bytes[0]&0x80 != 0 {
			c.AddUint8(0)
		}
		c.AddBytes(bytes)
	})
}

// from crypto/ecdsa
func encodeSignature(r, s []byte) ([]byte, error) {
	var b cryptobyte.Builder
	b.AddASN1(asn1.SEQUENCE, func(b *cryptobyte.Builder) {
		addASN1IntBytes(b, r)
		addASN1IntBytes(b, s)
	})
	return b.Bytes()
}

func (t *TPMSigner) Sign(_ io.Reader, digest []byte, opts crypto.SignerOpts) ([]byte, error) {
	if opts.HashFunc() != crypto.SHA256 {
		return nil, fmt.Errorf("incorrect checksum")
	}

	if len(digest) != 32 {
		return nil, fmt.Errorf("incorrect checksum length")
	}

	tpm := t.getTPM()
	defer tpm.Close()

	srkHandle, srkPublic, err := CreateSRK(tpm)
	if err != nil {
		return nil, fmt.Errorf("failed creating SRK: %v", err)
	}
	defer FlushHandle(tpm, srkHandle)

	handle, err := LoadKeyWithParent(tpm, *srkHandle, t.key)
	if err != nil {
		return nil, err
	}
	defer FlushHandle(tpm, handle)

	if t.key.PIN == HasPIN {
		p, err := t.pin(t.key)
		if err != nil {
			return nil, err
		}
		handle.Auth = tpm2.PasswordAuth(p)
	}

	sign := tpm2.Sign{
		KeyHandle: *handle,
		Digest:    tpm2.TPM2BDigest{Buffer: digest[:]},

		InScheme: tpm2.TPMTSigScheme{
			Scheme: tpm2.TPMAlgECDSA,
			Details: tpm2.NewTPMUSigScheme(
				tpm2.TPMAlgECDSA,
				&tpm2.TPMSSchemeHash{
					HashAlg: tpm2.TPMAlgSHA256,
				},
			),
		},
		Validation: tpm2.TPMTTKHashCheck{
			Tag: tpm2.TPMSTHashCheck,
		},
	}

	rspSign, err := sign.Execute(tpm,
		tpm2.HMAC(tpm2.TPMAlgSHA256, 16,
			tpm2.AESEncryption(128, tpm2.EncryptIn),
			tpm2.Salted(srkHandle.Handle, *srkPublic)))
	if err != nil {
		return nil, fmt.Errorf("failed to sign: %v", err)
	}

	eccsig, err := rspSign.Signature.Signature.ECDSA()
	if err != nil {
		return nil, fmt.Errorf("failed getting signature: %v", err)
	}

	return encodeSignature(eccsig.SignatureR.Buffer, eccsig.SignatureS.Buffer)
}

func createKey(tpm transport.TPMCloser, pin []byte) (*Key, error) {
	srkHandle, srkPublic, err := CreateSRK(tpm)
	if err != nil {
		return nil, fmt.Errorf("failed creating SRK: %v", err)
	}

	defer FlushHandle(tpm, srkHandle)

	// Template for en ECDSA key for signing
	eccKey := tpm2.Create{
		ParentHandle: srkHandle,
		InPublic: tpm2.New2B(tpm2.TPMTPublic{
			Type:    tpm2.TPMAlgECC,
			NameAlg: tpm2.TPMAlgSHA256,
			ObjectAttributes: tpm2.TPMAObject{
				SignEncrypt:         true,
				FixedTPM:            true,
				FixedParent:         true,
				SensitiveDataOrigin: true,
				UserWithAuth:        true,
			},
			Parameters: tpm2.NewTPMUPublicParms(
				tpm2.TPMAlgECC,
				&tpm2.TPMSECCParms{
					CurveID: tpm2.TPMECCNistP256,
					Scheme: tpm2.TPMTECCScheme{
						Scheme: tpm2.TPMAlgECDSA,
						Details: tpm2.NewTPMUAsymScheme(
							tpm2.TPMAlgECDSA,
							&tpm2.TPMSSigSchemeECDSA{
								HashAlg: tpm2.TPMAlgSHA256,
							},
						),
					},
				},
			),
		}),
	}

	pinstatus := NoPIN

	if !bytes.Equal(pin, []byte("")) {
		eccKey.InSensitive = tpm2.TPM2BSensitiveCreate{
			Sensitive: &tpm2.TPMSSensitiveCreate{
				UserAuth: tpm2.TPM2BAuth{
					Buffer: pin,
				},
			},
		}
		pinstatus = HasPIN
	}

	var eccRsp *tpm2.CreateResponse
	eccRsp, err = eccKey.Execute(tpm,
		tpm2.HMAC(tpm2.TPMAlgSHA256, 16,
			tpm2.AESEncryption(128, tpm2.EncryptIn),
			tpm2.Salted(srkHandle.Handle, *srkPublic)))
	if err != nil {
		return nil, fmt.Errorf("failed creating TPM key: %v", err)
	}

	return &Key{
		Version: 1,
		PIN:     pinstatus,
		Private: eccRsp.OutPrivate,
		Public:  eccRsp.OutPublic,
	}, nil
}

func LoadKeyWithParent(tpm transport.TPMCloser, parent tpm2.AuthHandle, key *Key) (*tpm2.AuthHandle, error) {
	loadBlobCmd := tpm2.Load{
		ParentHandle: parent,
		InPrivate:    key.Private,
		InPublic:     key.Public,
	}
	loadBlobRsp, err := loadBlobCmd.Execute(tpm)
	if err != nil {
		return nil, fmt.Errorf("failed getting handle: %v", err)
	}

	// Return a AuthHandle with a nil PasswordAuth
	return &tpm2.AuthHandle{
		Handle: loadBlobRsp.ObjectHandle,
		Name:   loadBlobRsp.Name,
		Auth:   tpm2.PasswordAuth(nil),
	}, nil
}

func LoadKey(tpm transport.TPMCloser, key *Key) (*tpm2.AuthHandle, error) {
	srkHandle, _, err := CreateSRK(tpm)
	if err != nil {
		return nil, err
	}

	defer FlushHandle(tpm, srkHandle)

	return LoadKeyWithParent(tpm, *srkHandle, key)
}

func SaveKey(key *Key) error {
	os.MkdirAll(getAgentStorage(), 0700)
	return os.WriteFile(path.Join(getAgentStorage(), "ssh.key"), MarshalKey(key), 0600)
}
