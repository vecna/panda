/*
Package panda implements the PANDA key agreement protocol.

PANDA is a protocol for two parties in possession of a shared secret to
exchange a short message (e.g. a public key) asynchronously. In order for the
process to be asynchronous, a server is assumed to exist which accepts messages
posted with a binary 'tag' and returns another message, if any, posted to the
same tag. Posting the same message to the same tag is idempotent. The server
should not evict one of the two messages posted to a tag if a third is
presented. Rather the third attempt should be rejected until the tag is garbage
collected after, say, a week.

The shared secret is assumed to be human memorable over the span of a few days.
It's processed with an expensive scrypt invocation to make it hard to
brute-force as it cannot be salted. Additionally, PANDA is a two round
protocol. In the first round, an iteration of SPAKE2 is performed to establish
a shared key and, in the second round, that shared key is used to pass the
messages.

This means that the messages cannot be decrypted after the fact by
brute-forcing the human-memorable secret. That is only valuable to an attacker
during the course of an exchange.
*/
package panda

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"io"
	"math/big"
	"strconv"

	"code.google.com/p/go.crypto/nacl/secretbox"
	"code.google.com/p/go.crypto/scrypt"
	"code.google.com/p/goprotobuf/proto"
	"github.com/agl/panda/stateproto"
)

// bodySize is the number of bytes that we'll pad every message to.
const bodySize = 1<<17
// MaxMessageLen is the maximum size of a message exchanged via PANDA.
const MaxMessageLen = bodySize - 24 /* nonce */ - secretbox.Overhead - 2

// groupP and groupG define the multiplicative group in which we perform
// SPAKE2. They are taken from
// https://tools.ietf.org/html/rfc3526#section-5.
var groupP, groupG *big.Int

// groupN is a verifiably random member of the group, generated by taking 4096
// bits of Salsa20 output with a zero nonce and where the key is
// SHA-256("PANDA key exchange, seed for N").
var groupN *big.Int

func init() {
	groupP, _ = new(big.Int).SetString("FFFFFFFFFFFFFFFFC90FDAA22168C234C4C6628B80DC1CD129024E088A67CC74020BBEA63B139B22514A08798E3404DDEF9519B3CD3A431B302B0A6DF25F14374FE1356D6D51C245E485B576625E7EC6F44C42E9A637ED6B0BFF5CB6F406B7EDEE386BFB5A899FA5AE9F24117C4B1FE649286651ECE45B3DC2007CB8A163BF0598DA48361C55D39A69163FA8FD24CF5F83655D23DCA3AD961C62F356208552BB9ED529077096966D670C354E4ABC9804F1746C08CA18217C32905E462E36CE3BE39E772C180E86039B2783A2EC07A28FB5C55DF06F4C52C9DE2BCBF6955817183995497CEA956AE515D2261898FA051015728E5A8AAAC42DAD33170D04507A33A85521ABDF1CBA64ECFB850458DBEF0A8AEA71575D060C7DB3970F85A6E1E4C7ABF5AE8CDB0933D71E8C94E04A25619DCEE3D2261AD2EE6BF12FFA06D98A0864D87602733EC86A64521F2B18177B200CBBE117577A615D6C770988C0BAD946E208E24FA074E5AB3143DB5BFCE0FD108E4B82D120A92108011A723C12A787E6D788719A10BDBA5B2699C327186AF4E23C1A946834B6150BDA2583E9CA2AD44CE8DBBBC2DB04DE8EF92E8EFC141FBECAA6287C59474E6BC05D99B2964FA090C3A2233BA186515BE7ED1F612970CEE2D7AFB81BDD762170481CD0069127D5B05AA993B4EA988D8FDDC186FFB7DC90A6C08F4DF435C934063199FFFFFFFFFFFFFFFF", 16)
	groupG = big.NewInt(2)
	groupN, _ = new(big.Int).SetString("a4fc1dc7a9a7fb350cbe7ca8301e69be1b0a7d904214218dcb055aa5a43f5d5eafed84f570fb13532075ada5aa2aa3cd52b84f3dcadcccc99f22cbcf8666eb768bbe7adda90709d73011d8474d6e4d458a5e0c9f61bce08b76f86707702787814b122b6f51352dfd69a5da48def271f814b09116e200b01e5acfc66f666f8268447eb0ec2aac64a97093f09908653f93c5723d38e404f0f01b46799b5ef398dd4bd9e4301d704dd22d2bc4de8fed055be9992b147ac686364d80dcd5153ea6e9fdb85a65d78fc70ce816f2fc964d270affe1cb5267fad6bd17ad1994de8854f6c68d1347db7c65250196fddbf0ebbea9e2c4ab2f82bc4784f3d36881bab1b5b05ebf1a758d24a7db1f2030607349bc0e961e82e1ca9301bd3fa1ce32364a1febf5bc9915aa364bf1c1ac62e066022cb9828fb39becf77dcb3d0b1db35ecfdf7cf91c381b355b74175b5fb2918008ad775132fb3886333449dfc55bb65417c2a0c45559370f66d0e955d1c28e46f7274639b039736546c502470513a1e36a793f888ce880b3fe00e83018049749fc4870cefbbb9a9a6e10f90a78cd0de85360f7b0d7abaab43d99d539b48afb56e36c8538c03faf43320324c76741d8c7ea419dea6de120bdbb93402284436645cc4b4d4190ee0313dc2302b31cb4eb55cb4c4d779b56ca9b91423a43b50868c5211caf9491f36b77abb0e29f98639ef6592e77", 16)
}

// Exchange represents a key exchange in progress.
type Exchange struct {
	key [32]byte
	x, X *big.Int
	haveSharedKey bool
	sharedKey [32]byte
	message []byte
}

var testingMode = false

// New creates a new Exchange that will send the given message to the other
// holder of the shared secret. It performs a significant amount of computation
// (many seconds).
func New(r io.Reader, secret, message []byte) (*Exchange, error) {
	if len(message) > MaxMessageLen {
		return nil, errors.New("panda: message too large")
	}

	var keySlice []byte
	var err error

	if testingMode {
		h := sha256.New()
		h.Write(secret)
		keySlice = h.Sum(nil)
	} else {
		if keySlice, err = scrypt.Key(secret, nil, 1<<16, 16, 4, 32); err != nil {
			return nil, err
		}
	}

	ex := &Exchange{
		message: message,
	}
	copy(ex.key[:], keySlice)

	for {
		if ex.x, err = rand.Int(r, groupP); err != nil {
			return nil, err
		}
		if ex.x.Sign() > 0 {
			break
		}
	}
	ex.X = new(big.Int).Exp(groupG, ex.x, groupP)
	ex.X.Mul(ex.X, ex.nPW())
	ex.X.Mod(ex.X, groupP)

	return ex, nil
}

// Unmarshal creates an Exchange from the result of calling Marshal.
func Unmarshal(data []byte) (*Exchange, error) {
	s := new(stateproto.State)
	if err := proto.Unmarshal(data, s); err != nil {
		return nil, err
	}
	ex := &Exchange{
		message: s.Message,
		x: new(big.Int).SetBytes(s.XBytes),
		X: new(big.Int).SetBytes(s.PublicBytes),
		haveSharedKey: len(s.SharedKey) > 0,
	}
	copy(ex.key[:], s.Key)
	if ex.haveSharedKey {
		copy(ex.sharedKey[:], s.SharedKey)
	}
	
	return ex, nil
}

// Marshal serializes the state of ex. The serialized data is not encrypted and
// contains secrets.
func (ex *Exchange) Marshal() []byte {
	var sharedKey []byte
	if ex.haveSharedKey {
		sharedKey = ex.sharedKey[:]
	}

	s, err := proto.Marshal(&stateproto.State{
		Key: ex.key[:],
		Message: ex.message,
		XBytes: ex.x.Bytes(),
		PublicBytes: ex.X.Bytes(),
		SharedKey: sharedKey,
	})
	if err != nil {
		panic(err)
	}
	return s
}

func deriveKey(key *[32]byte, context string) []byte {
	h := hmac.New(sha256.New, key[:])
	h.Write([]byte(context))
	h.Write(key[:])
	return h.Sum(nil)
}

func (ex *Exchange) nPW() *big.Int {
	return new(big.Int).Exp(groupN, new(big.Int).SetBytes(deriveKey(&ex.key, "spake")), groupP)
}

func padAndBox(key *[32]byte, body []byte) []byte {
	nonceSlice := deriveKey(key, string(body))
	var nonce [24]byte
	copy(nonce[:], nonceSlice)

	padded := make([]byte, bodySize - len(nonce) - secretbox.Overhead)
	padded[0] = byte(len(body))
	padded[1] = byte(len(body) >> 8)
	if n := copy(padded[2:], body); n < len(body) {
		panic("argument to padAndBox too large: " + strconv.Itoa(len(body)))
	}

	box := make([]byte, bodySize)
	copy(box, nonce[:])
	secretbox.Seal(box[len(nonce):len(nonce)], padded, &nonce, key)
	return box
}

func unbox(key *[32]byte, body []byte) ([]byte, error) {
	var nonce [24]byte
	if len(body) < len(nonce)+secretbox.Overhead+2 {
		return nil, errors.New("panda: reply from server is too short to be valid")
	}
	copy(nonce[:], body)
	unsealed, ok := secretbox.Open(nil, body[len(nonce):], &nonce, key)
	if !ok {
		return nil, errors.New("panda: failed to authenticate reply from server")
	}
	l := int(unsealed[0]) | int(unsealed[1]) << 8
	unsealed = unsealed[2:]
	if l > len(unsealed) {
		return nil, errors.New("panda: corrupt but authentic message found")
	}
	return unsealed[:l], nil
}

// NextRequest returns a tag and message for transmission to the shared server.
// NextRequest is idempotent.
func (ex *Exchange) NextRequest() (tag, body []byte) {
	if !ex.haveSharedKey {
		// First round: exchange SPAKE2 public values.
		tag = deriveKey(&ex.key, "round one tag")
		body = padAndBox(&ex.key, ex.X.Bytes())
	} else {
		// Second round: send encrypted message.
		tag = deriveKey(&ex.key, "round two tag")
		body = padAndBox(&ex.sharedKey, ex.message)
	}
	return
}

func lengthPrefix(n *big.Int) []byte {
	b := n.Bytes()
	return append([]byte{byte(len(b)), byte(len(b) >> 8)}, b...)
}

// Process processes a message from a peer (presumably exchanged via a shared
// server). It should always be called after the result of NextRequest has been
// transmitted. If the exchange is complete, it returns the peer's message.
// Once this occurs, no further actions are required for the peer to complete
// the exchange.
func (ex *Exchange) Process(reply []byte) ([]byte, error) {
	if !ex.haveSharedKey {
		// First round.
		body, err := unbox(&ex.key, reply)
		if err != nil {
			return nil, err
		}
		Y := new(big.Int).SetBytes(body)
		if Y.Sign() <= 0 || Y.Cmp(groupP) >= 0 {
			return nil, errors.New("panda: invalid SPAKE value from peer")
		}
		npwInv := new(big.Int).ModInverse(ex.nPW(), groupP)
		unmaskedY := npwInv.Mul(Y, npwInv)
		unmaskedY.Mod(unmaskedY, groupP)
		shared := npwInv.Exp(unmaskedY, ex.x, groupP)

		h := hmac.New(sha256.New, ex.key[:])
		a, b := ex.X, Y
		if a.Cmp(b) > 0 {
			a, b = b, a
		}
		h.Write(lengthPrefix(a))
		h.Write(lengthPrefix(b))
		h.Write(lengthPrefix(shared))
		sharedKey := h.Sum(nil)
		copy(ex.sharedKey[:], sharedKey)
		ex.haveSharedKey = true
		return nil, nil
	}

	body, err := unbox(&ex.sharedKey, reply)
	if err != nil {
		return nil, err
	}
	return body, nil
}
