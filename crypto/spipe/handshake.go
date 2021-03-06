// Package spipe handles establishing secure communication between two peers.

package spipe

import (
	"bytes"
	"errors"
	"fmt"
	"strings"

	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"hash"

	proto "github.com/jbenet/go-ipfs/Godeps/_workspace/src/code.google.com/p/goprotobuf/proto"
	ci "github.com/jbenet/go-ipfs/crypto"
	peer "github.com/jbenet/go-ipfs/peer"
	u "github.com/jbenet/go-ipfs/util"
)

var log = u.Logger("handshake")

// List of supported ECDH curves
var SupportedExchanges = "P-256,P-224,P-384,P-521"

// List of supported Ciphers
var SupportedCiphers = "AES-256,AES-128"

// List of supported Hashes
var SupportedHashes = "SHA256,SHA512,SHA1"

// ErrUnsupportedKeyType is returned when a private key cast/type switch fails.
var ErrUnsupportedKeyType = errors.New("unsupported key type")

// ErrClosed signals the closing of a connection.
var ErrClosed = errors.New("connection closed")

// handsahke performs initial communication over insecure channel to share
// keys, IDs, and initiate communication.
func (s *SecurePipe) handshake() error {
	// Generate and send Hello packet.
	// Hello = (rand, PublicKey, Supported)
	nonce := make([]byte, 16)
	_, err := rand.Read(nonce)
	if err != nil {
		return err
	}

	log.Debug("handshake: %s <--> %s", s.local, s.remote)
	myPubKey, err := s.local.PubKey.Bytes()
	if err != nil {
		return err
	}

	proposeMsg := new(Propose)
	proposeMsg.Rand = nonce
	proposeMsg.Pubkey = myPubKey
	proposeMsg.Exchanges = &SupportedExchanges
	proposeMsg.Ciphers = &SupportedCiphers
	proposeMsg.Hashes = &SupportedHashes

	encoded, err := proto.Marshal(proposeMsg)
	if err != nil {
		return err
	}

	// Send our Propose packet
	select {
	case s.insecure.Out <- encoded:
	case <-s.ctx.Done():
		return ErrClosed
	}

	// Parse their Propose packet and generate an Exchange packet.
	// Exchange = (EphemeralPubKey, Signature)
	var resp []byte
	select {
	case <-s.ctx.Done():
		return ErrClosed
	case resp = <-s.insecure.In:
	}

	// u.POut("received encoded handshake\n")
	proposeResp := new(Propose)
	err = proto.Unmarshal(resp, proposeResp)
	if err != nil {
		return err
	}

	// get remote identity
	remotePubKey, err := ci.UnmarshalPublicKey(proposeResp.GetPubkey())
	if err != nil {
		return err
	}

	// get or construct peer
	s.remote, err = getOrConstructPeer(s.peers, remotePubKey)
	if err != nil {
		return err
	}
	log.Debug("%s Remote Peer Identified as %s", s.local, s.remote)

	exchange, err := selectBest(SupportedExchanges, proposeResp.GetExchanges())
	if err != nil {
		return err
	}

	cipherType, err := selectBest(SupportedCiphers, proposeResp.GetCiphers())
	if err != nil {
		return err
	}

	hashType, err := selectBest(SupportedHashes, proposeResp.GetHashes())
	if err != nil {
		return err
	}

	// u.POut("Selected %s %s %s\n", exchange, cipherType, hashType)
	epubkey, genSharedKey, err := ci.GenerateEKeyPair(exchange) // Generate EphemeralPubKey

	var handshake bytes.Buffer // Gather corpus to sign.
	handshake.Write(encoded)
	handshake.Write(resp)
	handshake.Write(epubkey)

	exPacket := new(Exchange)

	exPacket.Epubkey = epubkey
	exPacket.Signature, err = s.local.PrivKey.Sign(handshake.Bytes())
	if err != nil {
		return err
	}

	exEncoded, err := proto.Marshal(exPacket)

	// send out Exchange packet
	select {
	case s.insecure.Out <- exEncoded:
	case <-s.ctx.Done():
		return ErrClosed
	}

	// Parse their Exchange packet and generate a Finish packet.
	// Finish = E('Finish')
	var resp1 []byte
	select {
	case <-s.ctx.Done():
		return ErrClosed
	case resp1 = <-s.insecure.In:
	}

	exchangeResp := new(Exchange)
	err = proto.Unmarshal(resp1, exchangeResp)
	if err != nil {
		return err
	}

	var theirHandshake bytes.Buffer
	theirHandshake.Write(resp)
	theirHandshake.Write(encoded)
	theirHandshake.Write(exchangeResp.GetEpubkey())

	// u.POut("Remote Peer Identified as %s\n", s.remote)
	ok, err := s.remote.PubKey.Verify(theirHandshake.Bytes(), exchangeResp.GetSignature())
	if err != nil {
		return err
	}

	if !ok {
		return errors.New("Bad signature!")
	}

	secret, err := genSharedKey(exchangeResp.GetEpubkey())
	if err != nil {
		return err
	}

	cmp := bytes.Compare(myPubKey, proposeResp.GetPubkey())
	mIV, tIV, mCKey, tCKey, mMKey, tMKey := ci.KeyStretcher(cmp, cipherType, hashType, secret)

	go s.handleSecureIn(hashType, tIV, tCKey, tMKey)
	go s.handleSecureOut(hashType, mIV, mCKey, mMKey)

	finished := []byte("Finished")

	// send finished msg
	select {
	case <-s.ctx.Done():
		return ErrClosed
	case s.Out <- finished:
	}

	// recv finished msg
	var resp2 []byte
	select {
	case <-s.ctx.Done():
		return ErrClosed
	case resp2 = <-s.In:
	}

	if bytes.Compare(resp2, finished) != 0 {
		return errors.New("Negotiation failed.")
	}

	log.Debug("%s handshake: Got node id: %s", s.local, s.remote)
	return nil
}

func makeMac(hashType string, key []byte) (hash.Hash, int) {
	switch hashType {
	case "SHA1":
		return hmac.New(sha1.New, key), sha1.Size
	case "SHA512":
		return hmac.New(sha512.New, key), sha512.Size
	default:
		return hmac.New(sha256.New, key), sha256.Size
	}
}

func (s *SecurePipe) handleSecureIn(hashType string, tIV, tCKey, tMKey []byte) {
	theirBlock, _ := aes.NewCipher(tCKey)
	theirCipher := cipher.NewCTR(theirBlock, tIV)

	theirMac, macSize := makeMac(hashType, tMKey)

	for {
		data, ok := <-s.insecure.In
		if !ok {
			close(s.Duplex.In)
			return
		}

		// log.Debug("[peer %s] secure in [from = %s] %d", s.local, s.remote, len(data))
		if len(data) <= macSize {
			continue
		}

		mark := len(data) - macSize
		buff := make([]byte, mark)

		theirCipher.XORKeyStream(buff, data[0:mark])

		theirMac.Write(data[0:mark])
		expected := theirMac.Sum(nil)
		theirMac.Reset()

		hmacOk := hmac.Equal(data[mark:], expected)

		if hmacOk {
			s.Duplex.In <- buff
		} else {
			s.Duplex.In <- nil
		}
	}
}

func (s *SecurePipe) handleSecureOut(hashType string, mIV, mCKey, mMKey []byte) {
	myBlock, _ := aes.NewCipher(mCKey)
	myCipher := cipher.NewCTR(myBlock, mIV)

	myMac, macSize := makeMac(hashType, mMKey)

	for {
		data, ok := <-s.Out
		if !ok {
			return
		}

		if len(data) == 0 {
			continue
		}

		buff := make([]byte, len(data)+macSize)

		myCipher.XORKeyStream(buff, data)

		myMac.Write(buff[0:len(data)])
		copy(buff[len(data):], myMac.Sum(nil))
		myMac.Reset()

		// log.Debug("[peer %s] secure out [to = %s] %d", s.local, s.remote, len(buff))
		s.insecure.Out <- buff
	}
}

// IDFromPubKey retrieves a Public Key from the peer given by pk
func IDFromPubKey(pk ci.PubKey) (peer.ID, error) {
	b, err := pk.Bytes()
	if err != nil {
		return nil, err
	}
	hash := u.Hash(b)
	return peer.ID(hash), nil
}

// Determines which algorithm to use.  Note:  f(a, b) = f(b, a)
func selectBest(myPrefs, theirPrefs string) (string, error) {
	// Person with greatest hash gets first choice.
	myHash := u.Hash([]byte(myPrefs))
	theirHash := u.Hash([]byte(theirPrefs))

	cmp := bytes.Compare(myHash, theirHash)
	var firstChoiceArr, secChoiceArr []string

	if cmp == -1 {
		firstChoiceArr = strings.Split(theirPrefs, ",")
		secChoiceArr = strings.Split(myPrefs, ",")
	} else if cmp == 1 {
		firstChoiceArr = strings.Split(myPrefs, ",")
		secChoiceArr = strings.Split(theirPrefs, ",")
	} else { // Exact same preferences.
		myPrefsArr := strings.Split(myPrefs, ",")
		return myPrefsArr[0], nil
	}

	for _, secChoice := range secChoiceArr {
		for _, firstChoice := range firstChoiceArr {
			if firstChoice == secChoice {
				return firstChoice, nil
			}
		}
	}

	return "", errors.New("No algorithms in common!")
}

// getOrConstructPeer attempts to fetch a peer from a peerstore.
// if succeeds, verify ID and PubKey match.
// else, construct it.
func getOrConstructPeer(peers peer.Peerstore, rpk ci.PubKey) (*peer.Peer, error) {

	rid, err := IDFromPubKey(rpk)
	if err != nil {
		return nil, err
	}

	npeer, err := peers.Get(rid)
	if err != nil || npeer == nil {
		if err != peer.ErrNotFound {
			return nil, err // unexpected error happened.
		}

		// dont have peer, so construct it + add it to peerstore.
		npeer = &peer.Peer{ID: rid, PubKey: rpk}
		if err := peers.Put(npeer); err != nil {
			return nil, err
		}

		// done, return the newly constructed peer.
		return npeer, nil
	}

	// did have it locally.

	// let's verify ID
	if !npeer.ID.Equal(rid) {
		e := "Expected peer.ID does not match sent pubkey's hash: %v - %v"
		return nil, fmt.Errorf(e, npeer, rid)
	}

	if npeer.PubKey == nil {
		// didn't have a pubkey, just set it.
		npeer.PubKey = rpk
		return npeer, nil
	}

	// did have pubkey, let's verify it's really the same.
	// this shouldn't ever happen, given we hashed, etc, but it could mean
	// expected code (or protocol) invariants violated.
	if !npeer.PubKey.Equals(rpk) {
		return nil, fmt.Errorf("WARNING: PubKey mismatch: %v", npeer)
	}
	return npeer, nil
}
