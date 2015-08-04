package twofactor

import (
	"bytes"
	"code.google.com/p/rsc/qr"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/url"
	"strconv"
	"time"
)

const (
	BACKOFF_MINUTES = 5 // this is the time to wait before verifying another token
	MAX_FAILURES    = 3 // total amount of failures, after that the user needs to wait for the backoff time
	COUNTER_SIZE    = 8 // this is defined in the RFC 4226
)

type totp struct {
	key                       []byte             // this is the secret key
	counter                   [COUNTER_SIZE]byte // this is the counter used to synchronize with the client device
	digits                    int                // total amount of digits of the code displayed on the device
	issuer                    string             // the company which issues the 2FA
	account                   string             // usually the suer email or the account id
	stepSize                  int                // by default 30 seconds
	clientOffset              int                // the amount of steps the client is off
	totalVerificationFailures int                // the total amount of verification failures from the client - by default 10
	lastVerificationTime      time.Time          // the last verification executed
	hashFunction              crypto.Hash        // the hash function used in the HMAC construction (sha1 - sha156 - sha512)
}

// This function is used to synchronize the counter with the client
// Offset can be a negative number as well
// Usually it's either -1, 0 or 1
// This is used internally
func (otp *totp) synchronizeCounter(offset int) {
	otp.clientOffset = offset
}

// Label returns the combination of issuer:account string
func (otp *totp) label() string {
	return url.QueryEscape(fmt.Sprintf("%s:%s", otp.issuer, otp.account))
}

// Counter returns the TOTP's 8-byte counter as unsigned 64-bit integer.
func (otp *totp) getIntCounter() uint64 {
	return uint64FromBigEndian(otp.counter)
}

// This function creates a new TOTP object
// This is the function which is needed to start the whole process
// account: usually the user email
// issuer: the name of the company/service
// hash: is the crypto function used: crypto.SHA1, crypto.SHA256, crypto.SHA512
// digits: is the token amount of digits (6 or 7 or 8)
// steps: the amount of second the token is valid
// it autmatically generates a secret key using the golang crypto rand package. If there is not enough entropy the function returns an error
// The key is not encrypted in this package. It's a secret key. Therefore if you transfer the key bytes in the network,
// please take care of protecting the key or in fact all the bytes.
func NewTOTP(account, issuer string, hash crypto.Hash, digits int) (*totp, error) {

	keySize := hash.Size()
	key := make([]byte, keySize)
	total, err := rand.Read(key)
	if err != nil {
		return nil, errors.New(fmt.Sprintf("TOTP failed to create because there is not enough entropy, we got only %d random bytes", total))
	}

	// sanitize the digits range otherwise it may create invalid tokens !
	if digits < 6 || digits > 8 {
		digits = 8
	}

	return makeTOTP(key, account, issuer, hash, digits)

}

// Private function which initialize the TOTP so that it's easier to unit test it
// Used internnaly
func makeTOTP(key []byte, account, issuer string, hash crypto.Hash, digits int) (*totp, error) {
	otp := new(totp)
	otp.key = key
	otp.account = account
	otp.issuer = issuer
	otp.digits = digits
	otp.stepSize = 30 // we set it to 30 seconds which is the recommended value from the RFC
	otp.clientOffset = 0
	otp.hashFunction = hash
	return otp, nil
}

// This function validates the user privided token
// It calculates 3 different tokens. The current one, one before now and one after now.
// The difference is driven by the TOTP step size
// Based on which of the 3 steps it succeeds to validates, the client offset is updated.
// It also updates the total amount of verification failures and the last time a verification happened in UTC time
// Returns an error in case of verification failure, with the reason
// There is a very basic method which protects from timing attacks, although if the step time used is low it should not be necessary
// An attacker can still learn the synchronization offset. This is however irrelevant because the attacker has then 30 seconds to
// guess the code and after 3 failures the function returns an error for the following 5 minutes
func (otp *totp) Validate(userCode string) error {

	// verify that the token is valid
	if userCode == "" {
		return errors.New("User provided token is empty")
	}

	// check against the total amount of failures
	if otp.totalVerificationFailures >= MAX_FAILURES && !validBackoffTime(otp.lastVerificationTime) {
		return errors.New("The verification is locked down, because of too many trials.")
	}

	if otp.totalVerificationFailures >= MAX_FAILURES && validBackoffTime(otp.lastVerificationTime) {
		// reset the total verification failures counter
		otp.totalVerificationFailures = 0
	}

	// calculate the sha256 of the user code
	userTokenHash := sha256.Sum256([]byte(userCode))
	userToken := hex.EncodeToString(userTokenHash[:])

	// 1 calculate the 3 tokens
	tokens := make([]string, 3)
	token0Hash := sha256.Sum256([]byte(calculateTOTP(otp, -1)))
	token1Hash := sha256.Sum256([]byte(calculateTOTP(otp, 0)))
	token2Hash := sha256.Sum256([]byte(calculateTOTP(otp, 1)))
	tokens[0] = hex.EncodeToString(token0Hash[:]) // sha256.Sum256() // 30 seconds ago token
	tokens[1] = hex.EncodeToString(token1Hash[:]) // current token
	tokens[2] = hex.EncodeToString(token2Hash[:]) // next 30 seconds token

	// if the current time token is valid then, no need to re-sync and return nil
	if tokens[1] == userToken {
		return nil
	}

	// if the let's say 30 seconds ago token is valid then return nil, but re-synchronize
	if tokens[0] == userToken {
		otp.synchronizeCounter(-1)
		return nil
	}

	// if the let's say 30 seconds ago token is valid then return nil, but re-synchronize
	if tokens[2] == userToken {
		otp.synchronizeCounter(1)
		return nil
	}

	otp.totalVerificationFailures++
	otp.lastVerificationTime = time.Now().UTC() // important to have it in UTC

	// if we got here everything is good
	return errors.New("Tokens mismatch.")
}

// Checks the time difference between the function call time and the parameter
// if the difference of time is greater than BACKOFF_MINUTES  it returns true, otherwise false
func validBackoffTime(lastVerification time.Time) bool {
	diff := lastVerification.UTC().Add(BACKOFF_MINUTES * time.Minute)
	return time.Now().UTC().After(diff)
}

// Basically, we define TOTP as TOTP = HOTP(K, T), where T is an integer
// and represents the number of time steps between the initial counter
// time T0 and the current Unix time.
// T = (Current Unix time - T0) / X, where the
// default floor function is used in the computation.
// For example, with T0 = 0 and Time Step X = 30, T = 1 if the current
// Unix time is 59 seconds, and T = 2 if the current Unix time is
// 60 seconds.
func (otp *totp) incrementCounter(index int) {
	// Unix returns t as a Unix time, the number of seconds elapsed since January 1, 1970 UTC.
	counterOffset := time.Duration(index*otp.stepSize) * time.Second
	clientOffset := time.Duration(otp.clientOffset*otp.stepSize) * time.Second
	now := time.Now().UTC().Add(counterOffset).Add(clientOffset).Unix()
	otp.counter = bigEndianUint64(increment(now, otp.stepSize))
}

// Function which calculates the value of T (see rfc6238)
func increment(ts int64, stepSize int) uint64 {
	T := float64(ts / int64(stepSize)) // TODO: improve this conversions
	n := round(T)                      // round T
	return n                           // convert n to big endian byte array
}

// Generates a new one time password with hmac-(HASH-FUNCTION)
func (otp *totp) OTP() string {
	// it uses the index 0, meaning that it calculates the current one
	return calculateTOTP(otp, 0)
}

// Private function which calculates the OTP token based on the index offset
// example: 1 * steps or -1 * steps
func calculateTOTP(otp *totp, index int) string {
	var h hash.Hash

	switch otp.hashFunction {
	case crypto.SHA256:
		h = hmac.New(sha256.New, otp.key)
		break
	case crypto.SHA512:
		h = hmac.New(sha512.New, otp.key)
		break
	default:
		h = hmac.New(sha1.New, otp.key)
		break

	}

	// set the counter to the current step based ont he current time
	// this is necessary to generate the proper OTP
	otp.incrementCounter(index)

	return calculateToken(otp.counter[:], otp.digits, h)

}

func truncateHash(hmac_result []byte, size int) int64 {
	offset := hmac_result[size-1] & 0xf
	bin_code := (uint32(hmac_result[offset])&0x7f)<<24 |
		(uint32(hmac_result[offset+1])&0xff)<<16 |
		(uint32(hmac_result[offset+2])&0xff)<<8 |
		(uint32(hmac_result[offset+3]) & 0xff)
	return int64(bin_code)
}

// this is the function which calculates the HTOP code
func calculateToken(counter []byte, digits int, h hash.Hash) string {

	h.Write(counter)
	hashResult := h.Sum(nil)
	result := truncateHash(hashResult, h.Size())
	var mod uint64
	if digits == 8 {
		mod = uint64(result % 100000000)
	}

	if digits == 7 {
		mod = uint64(result % 10000000)
	}

	if digits == 6 {
		mod = uint64(result % 1000000)
	}

	fmtStr := fmt.Sprintf("%%0%dd", digits)
	return fmt.Sprintf(fmtStr, mod)
}

// URL returns a suitable URL, such as for the Google Authenticator app
// example: otpauth://totp/Example:alice@google.com?secret=JBSWY3DPEHPK3PXP&issuer=Example
func (otp *totp) URL() string {
	secret := base32.StdEncoding.EncodeToString(otp.key)
	u := url.URL{}
	v := url.Values{}
	u.Scheme = "otpauth"
	u.Host = "totp"
	u.Path = otp.label()
	v.Add("secret", secret)
	v.Add("counter", fmt.Sprintf("%d", otp.getIntCounter()))
	v.Add("issuer", otp.issuer)
	v.Add("digits", strconv.Itoa(otp.digits))
	v.Add("period", strconv.Itoa(otp.stepSize))
	switch otp.hashFunction {
	case crypto.SHA256:
		v.Add("algorithm", "SHA256")
		break
	case crypto.SHA512:
		v.Add("algorithm", "SHA512")
		break
	default:
		v.Add("algorithm", "SHA1")
		break
	}
	u.RawQuery = v.Encode()
	return u.String()
}

// QR generates a byte array containing QR code encoded PNG image, with level Q error correction,
// needed for the client apps to generate tokens
// The QR code should be displayed only the first time the user enabled the Two-Factor authentication.
// The QR code contains the shared KEY between the server application and the client application,
// therefore the QR code should be delivered via secure connection.
func (otp *totp) QR() ([]byte, error) {
	u := otp.URL()
	code, err := qr.Encode(u, qr.Q)
	if err != nil {
		return nil, err
	}
	return code.PNG(), nil
}

// ToBytes serialises a TOTP object in a byte array
// Sizes:         4        4      N     8       4        4        N         4          N      4     4          4               8                 4
// Format: |total_bytes|key_size|key|counter|digits|issuer_size|issuer|account_size|account|steps|offset|total_failures|verification_time|hashFunction_type|
// hashFunction_type: 0 = SHA1; 1 = SHA256; 2 = SHA512
// TODO:
// 1- improve sizes. For instance the hashFunction_type could be a short.
// 2- Encrypt the key, in case it's transferred in the network unsafely
func (otp *totp) ToBytes() ([]byte, error) {
	var buffer bytes.Buffer

	// caluclate the length of the key and create its byte representation
	keySize := len(otp.key)
	keySizeBytes := bigEndianInt(keySize)

	// caluclate the length of the issuer and create its byte representation
	issuerSize := len(otp.issuer)
	issuerSizeBytes := bigEndianInt(issuerSize)

	// caluclate the length of the account and create its byte representation
	accountSize := len(otp.account)
	accountSizeBytes := bigEndianInt(accountSize)

	totalSize := 4 + 4 + keySize + 8 + 4 + 4 + issuerSize + 4 + accountSize + 4 + 4 + 4 + 8 + 4
	totalSizeBytes := bigEndianInt(totalSize)

	// at this point we are ready to write the data to the byte buffer
	// total size
	if _, err := buffer.Write(totalSizeBytes[:]); err != nil {
		return nil, err
	}

	// key
	if _, err := buffer.Write(keySizeBytes[:]); err != nil {
		return nil, err
	}
	if _, err := buffer.Write(otp.key); err != nil {
		return nil, err
	}

	// counter
	counterBytes := bigEndianUint64(otp.getIntCounter())
	if _, err := buffer.Write(counterBytes[:]); err != nil {
		return nil, err
	}

	// digits
	digitBytes := bigEndianInt(otp.digits)
	if _, err := buffer.Write(digitBytes[:]); err != nil {
		return nil, err
	}

	// issuer
	if _, err := buffer.Write(issuerSizeBytes[:]); err != nil {
		return nil, err
	}
	if _, err := buffer.WriteString(otp.issuer); err != nil {
		return nil, err
	}

	// account
	if _, err := buffer.Write(accountSizeBytes[:]); err != nil {
		return nil, err
	}
	if _, err := buffer.WriteString(otp.account); err != nil {
		return nil, err
	}

	// steps
	stepsBytes := bigEndianInt(otp.stepSize)
	if _, err := buffer.Write(stepsBytes[:]); err != nil {
		return nil, err
	}

	// offset
	offsetBytes := bigEndianInt(otp.clientOffset)
	if _, err := buffer.Write(offsetBytes[:]); err != nil {
		return nil, err
	}

	// total_failures
	totalFailuresBytes := bigEndianInt(otp.totalVerificationFailures)
	if _, err := buffer.Write(totalFailuresBytes[:]); err != nil {
		return nil, err
	}

	// last verification time
	verificationTimeBytes := bigEndianUint64(uint64(otp.lastVerificationTime.Unix()))
	if _, err := buffer.Write(verificationTimeBytes[:]); err != nil {
		return nil, err
	}

	// has_function_type
	switch otp.hashFunction {
	case crypto.SHA256:
		sha256Bytes := bigEndianInt(1)
		if _, err := buffer.Write(sha256Bytes[:]); err != nil {
			return nil, err
		}
		break
	case crypto.SHA512:
		sha512Bytes := bigEndianInt(2)
		if _, err := buffer.Write(sha512Bytes[:]); err != nil {
			return nil, err
		}
		break
	default:
		sha1Bytes := bigEndianInt(0)
		if _, err := buffer.Write(sha1Bytes[:]); err != nil {
			return nil, err
		}
	}

	//fmt.Println("Total bytes", len(buffer.Bytes()))
	return buffer.Bytes(), nil

}

// TOTPFromBytes converts a byte array to a totp object
// it stores the state of the TOTP object, like the key, the current counter, the client offset,
// the total amount of verification failures and the last time a verification happened
func TOTPFromBytes(data []byte) (*totp, error) {
	// fmt.Println("Bytes", len(data))
	// new reader
	reader := bytes.NewReader(data)

	// otp object
	otp := new(totp)

	// get the lenght
	lenght := make([]byte, 4)
	_, err := reader.Read(lenght) // read the 4 bytes for the total lenght
	if err != nil && err != io.EOF {
		return otp, err
	}

	totalSize := intFromBigEndian([4]byte{lenght[0], lenght[1], lenght[2], lenght[3]})
	buffer := make([]byte, totalSize-4)
	_, err = reader.Read(buffer)
	if err != nil && err != io.EOF {
		return otp, err
	}

	// skip the total bytes size
	startOffset := 0
	// read key size
	endOffset := startOffset + 4
	keyBytes := buffer[startOffset:endOffset]
	keySize := intFromBigEndian([4]byte{keyBytes[0], keyBytes[1], keyBytes[2], keyBytes[3]})

	// read the key
	startOffset = endOffset
	endOffset = startOffset + keySize
	otp.key = buffer[startOffset:endOffset]

	// read the counter
	startOffset = endOffset
	endOffset = startOffset + 8
	b := buffer[startOffset:endOffset]
	otp.counter = [8]byte{b[0], b[1], b[2], b[3], b[4], b[5], b[6], b[7]}

	// read the digits
	startOffset = endOffset
	endOffset = startOffset + 4
	b = buffer[startOffset:endOffset]
	otp.digits = intFromBigEndian([4]byte{b[0], b[1], b[2], b[3]}) //

	// read the issuer size
	startOffset = endOffset
	endOffset = startOffset + 4
	b = buffer[startOffset:endOffset]
	issuerSize := intFromBigEndian([4]byte{b[0], b[1], b[2], b[3]})

	// read the issuer string
	startOffset = endOffset
	endOffset = startOffset + issuerSize
	otp.issuer = string(buffer[startOffset:endOffset])

	// read the account size
	startOffset = endOffset
	endOffset = startOffset + 4
	b = buffer[startOffset:endOffset]
	accountSize := intFromBigEndian([4]byte{b[0], b[1], b[2], b[3]})

	// read the account string
	startOffset = endOffset
	endOffset = startOffset + accountSize
	otp.account = string(buffer[startOffset:endOffset])

	// read the steps
	startOffset = endOffset
	endOffset = startOffset + 4
	b = buffer[startOffset:endOffset]
	otp.stepSize = intFromBigEndian([4]byte{b[0], b[1], b[2], b[3]})

	// read the offset
	startOffset = endOffset
	endOffset = startOffset + 4
	b = buffer[startOffset:endOffset]
	otp.clientOffset = intFromBigEndian([4]byte{b[0], b[1], b[2], b[3]})

	// read the total failuers
	startOffset = endOffset
	endOffset = startOffset + 4
	b = buffer[startOffset:endOffset]
	otp.totalVerificationFailures = intFromBigEndian([4]byte{b[0], b[1], b[2], b[3]})

	// read the offset
	startOffset = endOffset
	endOffset = startOffset + 8
	b = buffer[startOffset:endOffset]
	ts := uint64FromBigEndian([8]byte{b[0], b[1], b[2], b[3], b[4], b[5], b[6], b[7]})
	otp.lastVerificationTime = time.Unix(int64(ts), 0)

	// read the hash type
	startOffset = endOffset
	endOffset = startOffset + 4
	b = buffer[startOffset:endOffset]
	hashType := intFromBigEndian([4]byte{b[0], b[1], b[2], b[3]})

	switch hashType {
	case 1:
		otp.hashFunction = crypto.SHA256
		break
	case 2:
		otp.hashFunction = crypto.SHA512
		break
	default:
		otp.hashFunction = crypto.SHA1
	}

	return otp, err
}