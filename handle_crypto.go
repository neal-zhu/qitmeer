// Copyright 2017-2018 The nox developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.
package main

import (
	"encoding/hex"
	"fmt"
	"github.com/HalalChain/qitmeer-lib/common/encode/base58"
	"github.com/HalalChain/qitmeer-lib/common/hash"
	"github.com/HalalChain/qitmeer-lib/crypto/bip32"
	"github.com/HalalChain/qitmeer-lib/crypto/bip39"
	"github.com/HalalChain/qitmeer-lib/crypto/ecc"
	"github.com/HalalChain/qitmeer-lib/crypto/seed"
	"github.com/HalalChain/qitmeer-lib/qx"
	"github.com/HalalChain/qitmeer-lib/wallet"
	"strconv"
)

func newEntropy(size uint) {
	s, err := seed.GenerateSeed(uint16(size))
	if err != nil {
		errExit(err)
	}
	fmt.Printf("%x\n", s)
}

func hdNewMasterPrivateKey(version bip32.Bip32Version, entropyStr string) {
	entropy, err := hex.DecodeString(entropyStr)
	if err != nil {
		errExit(err)
	}
	masterKey, err := bip32.NewMasterKey2(entropy, version)
	if err != nil {
		errExit(err)
	}
	fmt.Printf("%s\n", masterKey)
}

func hdPrivateKeyToHdPublicKey(version bip32.Bip32Version, privateKeyStr string) {
	data := base58.Decode(privateKeyStr)
	masterKey, err := bip32.Deserialize2(data, version)
	if err != nil {
		errExit(err)
	}
	if !masterKey.IsPrivate {
		errExit(fmt.Errorf("%s is not a HD (BIP32) private key", privateKeyStr))
	}
	pubKey := masterKey.PublicKey()
	fmt.Printf("%s\n", pubKey)
}

func hdKeyToEcKey(version bip32.Bip32Version, keyStr string) {
	data := base58.Decode(keyStr)
	key, err := bip32.Deserialize2(data, version)
	if err != nil {
		errExit(err)
	}
	if key.IsPrivate {
		fmt.Printf("%x\n", key.Key[:])
	} else {
		fmt.Printf("%x\n", key.PublicKey().Key[:])
	}
}

const bip32_ByteSize = 78 + 4

// The Serialization format of BIP32 Key
// https://github.com/bitcoin/bips/blob/master/bip-0032.mediawiki#serialization-format
//  4 bytes: version bytes
//           mainnet: 0x0488B21E public, 0x0488ADE4 private; testnet: 0x043587CF public, 0x04358394 private
//  1 byte : depth: 0x00 for master nodes, 0x01 for level-1 derived keys, ....
//  4 bytes: the fingerprint of the parent's key (0x00000000 if master key)
//  4 bytes: child number. This is ser32(i) for i in xi = xpar/i, with xi the key being serialized. (0x00000000 if master key)
//           index ≥ 0x80000000 to hardened keys
// 32 bytes: the chain code
// 33 bytes: the public key or private key data (serP(K) for public keys, 0x00 || ser256(k) for private keys)
//  4 bytes: checksum
func hdDecode(keyStr string) {
	data := base58.Decode(keyStr)
	if len(data) != bip32_ByteSize {
		errExit(fmt.Errorf("invalid bip32 key size (%d), the size hould be %d", len(data), bip32_ByteSize))
	}
	fmt.Printf("   version : %x (%s)\n", data[:4], getBip32NetworkInfo(data[:4]))
	fmt.Printf("     depth : %x\n", data[4:4+1])
	fmt.Printf(" parent fp : %x\n", data[5:5+4])
	childNumber, err := strconv.ParseInt(fmt.Sprintf("%x", data[9:9+4]), 16, 64)
	if err != nil {
		errExit(err)
	}
	hardened := childNumber >= 0x80000000
	fmt.Printf("  hardened : %v\n", hardened)
	if hardened {
		childNumber -= 0x80000000
	}
	fmt.Printf(" child num : %d (%x)\n", childNumber, data[9:9+4])
	fmt.Printf("chain code : %x\n", data[13:13+32])
	if keyStr[1:4] == "pub" {
		// the pub key should be 33 bytes,
		// the first byte 0x02 means y is even,
		// the first byte 0x03 means y is odd
		var oldOrEven string
		switch data[45] {
		case 0x02:
			oldOrEven = "even"
		case 0x03:
			oldOrEven = "odd"
		default:
			errExit(fmt.Errorf("invaid pub key [%x][%x]", data[45:46], data[46:46+32]))
		}
		fmt.Printf("   pub key : [%x][%x] y=%s\n", data[45:46], data[46:46+32], oldOrEven)
	} else {
		//the prv key should be 32 bytes, the first byte always 0x00
		fmt.Printf("   prv key : [%x][%x]\n", data[45:46], data[46:46+32])
	}
	fmt.Printf("  checksum : %x\n", data[78:78+4])
	fmt.Printf("       hex : %x\n", data[:78+4])
	fmt.Printf("    base58 : %s\n", keyStr)

}

func hdDerive(hard bool, index uint32, path wallet.DerivationPath, version bip32.Bip32Version, key string) {
	data := base58.Decode(key)
	if len(data) != bip32_ByteSize {
		errExit(fmt.Errorf("invalid bip32 key size (%d), the size hould be %d", len(data), bip32_ByteSize))
	}
	mKey, err := bip32.Deserialize2(data, version)
	if err != nil {
		errExit(err)
	}
	var childKey *bip32.Key
	if path.String() != "m" {
		var ck = mKey
		for _, i := range path {
			ck, err = ck.NewChildKey(i)
			if err != nil {
				errExit(err)
			}
		}
		childKey = ck
	} else {
		if hard {
			childKey, err = mKey.NewChildKey(bip32.FirstHardenedChild + index)
		} else {
			childKey, err = mKey.NewChildKey(index)
		}
		if err != nil {
			errExit(err)
		}
	}
	fmt.Printf("%s\n", childKey)
}

func mnemonicNew(entropyStr string) {
	entropy, err := hex.DecodeString(entropyStr)
	if err != nil {
		errExit(err)
	}
	mnemonic, err := bip39.NewMnemonic(entropy)
	if err != nil {
		errExit(err)
	}
	fmt.Printf("%s\n", mnemonic)
}

func mnemonicToEntropy(mnemonicStr string) {
	entropy, err := bip39.EntropyFromMnemonic(mnemonicStr)
	if err != nil {
		errExit(err)
	}
	fmt.Printf("%x\n", entropy)
}

func mnemonicToSeed(passphrase string, mnemonicStr string) {
	seed, err := bip39.NewSeedWithErrorChecking(mnemonicStr, passphrase)
	if err != nil {
		errExit(err)
	}
	fmt.Printf("%x\n", seed)
}

func ecNew(curve string, entropyStr string) {
	pk, err := qx.EcNew(curve, entropyStr)
	if err != nil {
		errExit(err)
	}
	fmt.Printf("%s\n", pk)
}

func ecPrivateKeyToEcPublicKey(uncompressed bool, privateKeyStr string) {
	key, err := qx.EcPrivateKeyToEcPublicKey(uncompressed, privateKeyStr)
	if err != nil {
		errExit(err)
	}
	fmt.Printf("%s\n", key)
}

func ecPrivateKeyToWif(uncompressed bool, privateKeyStr string) {
	data, err := hex.DecodeString(privateKeyStr)
	if err != nil {
		errExit(err)
	}
	privkey, _ := ecc.Secp256k1.PrivKeyFromBytes(data)
	var key []byte
	if uncompressed {
		key = privkey.Serialize()
	} else {
		key = privkey.Serialize()
		key = append(key, []byte{0x01}...)
	}
	cksumfunc := base58.DoubleHashChecksumFunc(hash.GetHasher(hash.SHA256), 4)
	encoded := base58.CheckEncode(key, []byte{0x80}, 4, cksumfunc)
	fmt.Printf("%s\n", encoded)
}

func wifToEcPrivateKey(wif string) {
	decoded, _, err := decodeWIF(wif)
	if err != nil {
		errExit(err)
	}
	fmt.Printf("%x\n", decoded)
}

func decodeWIF(wif string) ([]byte, bool, error) {
	cksumfunc := base58.DoubleHashChecksumFunc(hash.GetHasher(hash.SHA256), 4)
	decoded, version, err := base58.CheckDecode(wif, 1, 4, cksumfunc)
	compressed := false
	if err != nil {
		return nil, compressed, err
	}
	if len(version) != 1 && version[0] != 0x80 {
		return nil, compressed, fmt.Errorf("incorrect wif version %x, should be 0x80", version)
	}
	if len(decoded) == 32 {
		return decoded[:], compressed, nil
	} else if len(decoded) == 33 && decoded[32] == 0x01 {
		compressed = true
		return decoded[:32], compressed, nil
	} else {
		return nil, compressed, fmt.Errorf("incorrect wif length")
	}
}

func wifToEcPubkey(uncompressed bool, wif string) {
	decoded, _, err := decodeWIF(wif)
	if err != nil {
		errExit(err)
	}
	_, pubKey := ecc.Secp256k1.PrivKeyFromBytes(decoded)
	var key []byte
	if uncompressed {
		key = pubKey.SerializeUncompressed()
	} else {
		key = pubKey.SerializeCompressed()
	}
	fmt.Printf("%x\n", key[:])
}
