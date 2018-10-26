// Copyright 2016 David Lazar. All rights reserved.
// Use of this source code is governed by the GNU AGPL
// license that can be found in the LICENSE file.

package pkg

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"time"

	"github.com/dgraph-io/badger"
	"golang.org/x/crypto/ed25519"
	"golang.org/x/crypto/nacl/box"

	"vuvuzela.io/alpenhorn/log"
	"vuvuzela.io/crypto/bls"
	"vuvuzela.io/crypto/ibe"
)

type extractArgs struct {
	Round    uint32
	Username string

	// ReturnKey is a box key that is used to encrypt the
	// extracted IBE private key.
	ReturnKey *[32]byte

	// UserLongTermKey is the user's long-term signing key.
	// The PKG attests to this key in the extractReply.
	UserLongTermKey ed25519.PublicKey

	// ServerSigningKey ensures the request is tied to a single PKG.
	// This field is set locally by the client and server, so it does
	// not need to be included in the JSON request.
	ServerSigningKey ed25519.PublicKey `json:"-"`

	// Signature signs everything above with the user's login key.
	Signature []byte
}

func (a *extractArgs) Sign(loginKey ed25519.PrivateKey) {
	a.Signature = ed25519.Sign(loginKey, a.msg())
}

func (a *extractArgs) Verify(loginKey ed25519.PublicKey) bool {
	return ed25519.Verify(loginKey, a.msg(), a.Signature)
}

func (a *extractArgs) msg() []byte {
	buf := new(bytes.Buffer)
	buf.WriteString("ExtractArgs")
	buf.Write(a.ServerSigningKey)
	binary.Write(buf, binary.BigEndian, a.Round)
	id := ValidUsernameToIdentity(a.Username)
	buf.Write(id[:])
	buf.Write(a.ReturnKey[:])
	buf.Write(a.UserLongTermKey)
	return buf.Bytes()
}

type extractReply struct {
	Round               uint32
	Username            string
	EncryptedPrivateKey []byte
	Signature           []byte
	IdentitySig         bls.Signature
}

func (r *extractReply) Sign(key ed25519.PrivateKey) {
	r.Signature = ed25519.Sign(key, r.msg())
}

func (r *extractReply) Verify(key ed25519.PublicKey) bool {
	return ed25519.Verify(key, r.msg(), r.Signature)
}

func (r *extractReply) msg() []byte {
	buf := new(bytes.Buffer)
	buf.WriteString("ExtractReply")
	binary.Write(buf, binary.BigEndian, r.Round)
	id := ValidUsernameToIdentity(r.Username)
	buf.Write(id[:])
	buf.Write(r.EncryptedPrivateKey)
	return buf.Bytes()
}

func (srv *Server) extractHandler(w http.ResponseWriter, req *http.Request) {
	body := http.MaxBytesReader(w, req.Body, 1024)
	args := new(extractArgs)
	err := json.NewDecoder(body).Decode(args)
	if err != nil {
		httpError(w, errorf(ErrBadRequestJSON, "%s", err))
		return
	}
	args.ServerSigningKey = srv.publicKey

	reply, err := srv.extract(args)
	if err != nil {
		if isInternalError(err) {
			srv.log.WithFields(log.Fields{
				"round":    args.Round,
				"username": args.Username,
				"code":     errorCode(err).String(),
			}).Errorf("Extraction failed: %s", err)
		}
		httpError(w, err)
		return
	}

	bs, err := json.Marshal(reply)
	if err != nil {
		panic(err)
	}
	w.Write(bs)
}

// An Attestation attests that UserLongTermKey belongs to UserIdentity.
// The attestation is signed using the AttestKey from a PKG server.
type Attestation struct {
	// AttestKey is included in the attestation message to satisfy the
	// BLS requirement that messages must be distinct.
	AttestKey       *bls.PublicKey
	UserIdentity    *[64]byte
	UserLongTermKey ed25519.PublicKey
}

func (a *Attestation) Marshal() []byte {
	blsKeyBytes, _ := a.AttestKey.MarshalBinary()
	buf := new(bytes.Buffer)
	buf.Write(blsKeyBytes)
	buf.Write(a.UserIdentity[:])
	buf.Write([]byte(a.UserLongTermKey))
	return buf.Bytes()
}

var zeroNonce = new([24]byte)

func (srv *Server) extract(args *extractArgs) (*extractReply, error) {
	srv.mu.Lock()
	st, ok := srv.rounds[args.Round]
	srv.mu.Unlock()
	if !ok {
		return nil, errorf(ErrRoundNotFound, "%d", args.Round)
	}

	if len(args.UserLongTermKey) != ed25519.PublicKeySize {
		return nil, errorf(
			ErrInvalidUserLongTermKey,
			"got %d bytes, want %d",
			len(args.UserLongTermKey),
			ed25519.PublicKeySize,
		)
	}

	user, id, err := srv.getUser(nil, args.Username)
	if err != nil {
		return nil, err
	}
	if !args.Verify(user.LoginKey) {
		return nil, errorf(ErrInvalidSignature, "key=%x", user.LoginKey)
	}

	lastExtraction := lastExtraction{
		Round:    args.Round,
		UnixTime: time.Now().Unix(),
	}
	err = srv.db.Update(func(tx *badger.Txn) error {
		key := dbUserKey(id, lastExtractionSuffix)
		return tx.Set(key, lastExtraction.Marshal())
	})
	if err != nil {
		return nil, errorf(ErrDatabaseError, "%s", err)
	}

	idKeyBytes, _ := ibe.Extract(st.masterPrivateKey, id[:]).MarshalBinary()
	publicKey, privateKey, err := box.GenerateKey(rand.Reader)
	if err != nil {
		panic("box.GenerateKey: " + err.Error())
	}
	ctxt := box.Seal(publicKey[:], idKeyBytes, zeroNonce, args.ReturnKey, privateKey)

	attestation := &Attestation{
		AttestKey:       st.blsPublicKey,
		UserIdentity:    id,
		UserLongTermKey: args.UserLongTermKey,
	}
	idSig := bls.Sign(st.blsPrivateKey, attestation.Marshal())

	reply := &extractReply{
		Round:               args.Round,
		Username:            args.Username,
		EncryptedPrivateKey: ctxt,
		IdentitySig:         idSig,
	}
	reply.Sign(srv.privateKey)

	return reply, nil
}

func (srv *Server) getUser(tx *badger.Txn, username string) (user userState, id *[64]byte, err error) {
	id, err = UsernameToIdentity(username)
	if err != nil {
		return user, id, errorf(ErrInvalidUsername, "%s", err)
	}

	if tx == nil {
		tx = srv.db.NewTransaction(false)
		defer tx.Discard()
	}

	item, err := tx.Get(dbUserKey(id, registrationSuffix))
	if err == badger.ErrKeyNotFound {
		return user, id, errorf(ErrNotRegistered, "%q", username)
	}
	if err != nil {
		return user, id, errorf(ErrDatabaseError, "%s", err)
	}
	err = item.Value(func(data []byte) error {
		return user.Unmarshal(data)
	})
	if err != nil {
		return user, id, errorf(ErrDatabaseError, "%s", err)
	}
	return user, id, nil
}
