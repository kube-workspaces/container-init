package config

import (
	"crypto/ecdh"
	"encoding/binary"
	"math/big"
)

// Validate the supported SSH wire formats, not just the base64/header. Bad key
// material must fail with the rest of the document before provisioning starts.
func validSSHBlob(kind string, blob []byte) bool {
	field := func() []byte {
		if len(blob) < 4 {
			blob = nil
			return nil
		}
		n := uint64(binary.BigEndian.Uint32(blob[:4]))
		if n == 0 || n > uint64(len(blob)-4) {
			blob = nil
			return nil
		}
		value := blob[4 : 4+int(n)]
		blob = blob[4+int(n):]
		return value
	}
	if string(field()) != kind {
		return false
	}
	switch kind {
	case "ssh-ed25519", "sk-ssh-ed25519@openssh.com":
		if len(field()) != 32 {
			return false
		}
	case "ssh-rsa":
		e, n := field(), field()
		if len(e) == 0 || len(n) == 0 || e[0]&0x80 != 0 || n[0]&0x80 != 0 {
			return false
		}
		exponent, modulus := new(big.Int).SetBytes(e), new(big.Int).SetBytes(n)
		if exponent.BitLen() > 31 || exponent.Cmp(big.NewInt(3)) < 0 || exponent.Bit(0) == 0 || modulus.Sign() <= 0 || modulus.Bit(0) == 0 {
			return false
		}
	default:
		var curve ecdh.Curve
		var name string
		switch kind {
		case "ecdsa-sha2-nistp256", "sk-ecdsa-sha2-nistp256@openssh.com":
			curve, name = ecdh.P256(), "nistp256"
		case "ecdsa-sha2-nistp384":
			curve, name = ecdh.P384(), "nistp384"
		case "ecdsa-sha2-nistp521":
			curve, name = ecdh.P521(), "nistp521"
		default:
			return false
		}
		if string(field()) != name {
			return false
		}
		if _, err := curve.NewPublicKey(field()); err != nil {
			return false
		}
	}
	if kind == "sk-ssh-ed25519@openssh.com" || kind == "sk-ecdsa-sha2-nistp256@openssh.com" {
		if len(field()) == 0 {
			return false
		}
	}
	return len(blob) == 0
}
