package pdf

import (
	"bytes"
	"crypto/md5"
	"crypto/rc4"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
)

var pdfPasswordPadding = []byte{
	0x28, 0xbf, 0x4e, 0x5e, 0x4e, 0x75, 0x8a, 0x41,
	0x64, 0x00, 0x4e, 0x56, 0xff, 0xfa, 0x01, 0x08,
	0x2e, 0x2e, 0x00, 0xb6, 0xd0, 0x68, 0x3e, 0x80,
	0x2f, 0x0c, 0xa9, 0xfe, 0x64, 0x53, 0x69, 0x7a,
}

func decryptPDFObjects(file []byte, objects map[int]object) error {
	match := regexp.MustCompile(`/Encrypt\s+(\d+)\s+(\d+)\s+R`).FindSubmatch(file)
	if len(match) < 3 {
		return nil
	}
	encryptID, _ := strconv.Atoi(string(match[1]))
	encryptGeneration, _ := strconv.Atoi(string(match[2]))
	encryption, ok := objects[encryptID]
	if !ok || encryption.gen != encryptGeneration {
		return fmt.Errorf("PDF encryption dictionary is missing")
	}
	key, err := standardSecurityKey(file, encryption.dict, nil)
	if err != nil {
		return err
	}
	for id, current := range objects {
		if id == encryptID {
			continue
		}
		current.dict, err = decryptPDFStrings(current.dict, key, current.num, current.gen)
		if err != nil {
			return fmt.Errorf("decrypt strings in PDF object %d: %w", current.num, err)
		}
		if len(current.data) > 0 {
			current.data, err = decryptRC4Object(current.data, key, current.num, current.gen)
			if err != nil {
				return fmt.Errorf("decrypt PDF object %d stream: %w", current.num, err)
			}
		}
		objects[id] = current
	}
	return nil
}

func decryptPDFStrings(dictionary, fileKey []byte, objectNumber, generation int) ([]byte, error) {
	var output bytes.Buffer
	for i := 0; i < len(dictionary); {
		if i+1 < len(dictionary) && dictionary[i] == '<' && dictionary[i+1] == '<' {
			output.Write(dictionary[i : i+2])
			i += 2
			continue
		}
		if dictionary[i] == '%' {
			end := bytes.IndexByte(dictionary[i:], '\n')
			if end < 0 {
				output.Write(dictionary[i:])
				break
			}
			output.Write(dictionary[i : i+end+1])
			i += end + 1
			continue
		}
		start := i
		if dictionary[i] == '(' {
			depth := 1
			i++
			for i < len(dictionary) && depth > 0 {
				if dictionary[i] == '\\' {
					i += 2
					continue
				}
				if dictionary[i] == '(' {
					depth++
				} else if dictionary[i] == ')' {
					depth--
				}
				i++
			}
			if depth != 0 {
				return nil, fmt.Errorf("unterminated encrypted PDF string")
			}
		} else if dictionary[i] == '<' {
			i++
			for i < len(dictionary) && dictionary[i] != '>' {
				i++
			}
			if i >= len(dictionary) {
				return nil, fmt.Errorf("unterminated encrypted PDF hex string")
			}
			i++
		} else {
			output.WriteByte(dictionary[i])
			i++
			continue
		}
		raw := pdfStringBytes(string(dictionary[start:i]))
		decrypted, err := decryptRC4Object(raw, fileKey, objectNumber, generation)
		if err != nil {
			return nil, err
		}
		output.WriteByte('<')
		hexBuffer := make([]byte, hex.EncodedLen(len(decrypted)))
		hex.Encode(hexBuffer, decrypted)
		output.Write(hexBuffer)
		output.WriteByte('>')
	}
	return output.Bytes(), nil
}

func standardSecurityKey(file, dictionary, password []byte) ([]byte, error) {
	filter := dictName(dictionary, "Filter")
	version := int(dictNum(dictionary, "V"))
	revision := int(dictNum(dictionary, "R"))
	if filter != "Standard" || version < 1 || version > 2 || revision < 2 || revision > 3 {
		return nil, fmt.Errorf("unsupported encrypted PDF security handler (Filter=%s V=%d R=%d)", filter, version, revision)
	}
	owner := dictionaryString(dictionary, "O")
	user := dictionaryString(dictionary, "U")
	fileID := firstTrailerID(file)
	if len(owner) != 32 || len(user) != 32 || len(fileID) == 0 {
		return nil, fmt.Errorf("invalid PDF encryption dictionary")
	}
	permissions, ok := signedDictInteger(dictionary, "P")
	if !ok {
		return nil, fmt.Errorf("PDF encryption permissions are missing")
	}
	keyLength := 5
	if revision >= 3 {
		keyLength = int(dictNum(dictionary, "Length")) / 8
		if keyLength < 5 || keyLength > 16 {
			return nil, fmt.Errorf("unsupported PDF encryption key length: %d", keyLength*8)
		}
	}
	padded := make([]byte, 32)
	copy(padded, password)
	if len(password) < len(padded) {
		copy(padded[len(password):], pdfPasswordPadding[:len(padded)-len(password)])
	}
	var permissionBytes [4]byte
	binary.LittleEndian.PutUint32(permissionBytes[:], uint32(int32(permissions)))
	hashInput := make([]byte, 0, len(padded)+len(owner)+4+len(fileID))
	hashInput = append(hashInput, padded...)
	hashInput = append(hashInput, owner...)
	hashInput = append(hashInput, permissionBytes[:]...)
	hashInput = append(hashInput, fileID...)
	digest := md5.Sum(hashInput)
	key := append([]byte(nil), digest[:keyLength]...)
	if revision >= 3 {
		for i := 0; i < 50; i++ {
			next := md5.Sum(key)
			copy(key, next[:keyLength])
		}
	}
	if !validUserPassword(user, fileID, key, revision) {
		return nil, fmt.Errorf("encrypted PDF requires a password")
	}
	return key, nil
}

func validUserPassword(user, fileID, key []byte, revision int) bool {
	if revision == 2 {
		decoded, err := applyRC4(pdfPasswordPadding, key)
		return err == nil && bytes.Equal(decoded, user)
	}
	digestInput := append(append([]byte(nil), pdfPasswordPadding...), fileID...)
	digest := md5.Sum(digestInput)
	value, err := applyRC4(digest[:], key)
	if err != nil {
		return false
	}
	for i := byte(1); i <= 19; i++ {
		iterationKey := make([]byte, len(key))
		for j := range key {
			iterationKey[j] = key[j] ^ i
		}
		value, err = applyRC4(value, iterationKey)
		if err != nil {
			return false
		}
	}
	return bytes.Equal(value[:16], user[:16])
}

func decryptRC4Object(data, fileKey []byte, objectNumber, generation int) ([]byte, error) {
	seed := make([]byte, 0, len(fileKey)+5)
	seed = append(seed, fileKey...)
	seed = append(seed, byte(objectNumber), byte(objectNumber>>8), byte(objectNumber>>16))
	seed = append(seed, byte(generation), byte(generation>>8))
	digest := md5.Sum(seed)
	keyLength := len(fileKey) + 5
	if keyLength > 16 {
		keyLength = 16
	}
	return applyRC4(data, digest[:keyLength])
}

func applyRC4(data, key []byte) ([]byte, error) {
	cipher, err := rc4.NewCipher(key)
	if err != nil {
		return nil, err
	}
	result := make([]byte, len(data))
	cipher.XORKeyStream(result, data)
	return result, nil
}

func dictionaryString(dictionary []byte, key string) []byte {
	tokens := tokenize(string(dictionary))
	wanted := "/" + key
	for i := 0; i+1 < len(tokens); i++ {
		if tokens[i] == wanted {
			return pdfStringBytes(tokens[i+1])
		}
	}
	return nil
}

func firstTrailerID(file []byte) []byte {
	match := regexp.MustCompile(`(?s)/ID\s*(\[.*?\])`).FindSubmatch(file)
	if len(match) < 2 {
		return nil
	}
	items := parseArrayItems(string(match[1]))
	if len(items) == 0 {
		return nil
	}
	return pdfStringBytes(items[0])
}

func signedDictInteger(dictionary []byte, key string) (int64, bool) {
	match := regexp.MustCompile(`/` + regexp.QuoteMeta(key) + `\s+(-?\d+)`).FindSubmatch(dictionary)
	if len(match) < 2 {
		return 0, false
	}
	value, err := strconv.ParseInt(string(match[1]), 10, 32)
	return value, err == nil
}

func dictName(dictionary []byte, key string) string {
	match := regexp.MustCompile(`/` + regexp.QuoteMeta(key) + `\s*/([^\s/<>()\[\]]+)`).FindSubmatch(dictionary)
	if len(match) < 2 {
		return ""
	}
	return string(match[1])
}
