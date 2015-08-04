package twofactor

import (
	"bytes"
	"crypto"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"net/url"
	"testing"
	"time"
)

var sha1KeyHex = "3132333435363738393031323334353637383930"
var sha256KeyHex = "3132333435363738393031323334353637383930313233343536373839303132"
var sha512KeyHex = "31323334353637383930313233343536373839303132333435363738393031323334353637383930313233343536373839303132333435363738393031323334"

var sha1TestData = []string{
	"94287082",
	"07081804",
	"14050471",
	"89005924",
	"69279037",
	"65353130",
}

var sha256TestData = []string{
	"46119246",
	"68084774",
	"67062674",
	"91819424",
	"90698825",
	"77737706",
}

var sha512TestData = []string{
	"90693936",
	"25091201",
	"99943326",
	"93441116",
	"38618901",
	"47863826",
}

var timeCounters = []int64{
	int64(59),          // 1970-01-01 00:00:59
	int64(1111111109),  // 2005-03-18 01:58:29
	int64(1111111111),  // 2005-03-18 01:58:31
	int64(1234567890),  // 2009-02-13 23:31:30
	int64(2000000000),  // 2033-05-18 03:33:20
	int64(20000000000), // 2603-10-11 11:33:20
}

func checkError(t *testing.T, err error) {
	if err != nil {
		t.Fatal(err)
	}
}

func TestTOTP(t *testing.T) {

	keySha1, err := hex.DecodeString(sha1KeyHex)
	checkError(t, err)

	keySha256, err := hex.DecodeString(sha256KeyHex)
	checkError(t, err)

	keySha512, err := hex.DecodeString(sha512KeyHex)
	checkError(t, err)

	// create the OTP
	otp := new(totp)
	otp.digits = 8
	otp.issuer = "Sec51"
	otp.account = "no-reply@sec51.com"

	// Test SHA1
	otp.key = keySha1
	for index, ts := range timeCounters {
		counter := increment(ts, 30)
		otp.counter = bigEndianUint64(counter)
		hash := hmac.New(sha1.New, otp.key)
		token := calculateToken(otp.counter[:], otp.digits, hash)
		expected := sha1TestData[index]
		if token != expected {
			t.Errorf("SHA1 test data, token mismatch. Got %s, expected %s\n", token, expected)
		}
	}

	// Test SHA256
	otp.key = keySha256
	for index, ts := range timeCounters {
		counter := increment(ts, 30)
		otp.counter = bigEndianUint64(counter)
		hash := hmac.New(sha256.New, otp.key)
		token := calculateToken(otp.counter[:], otp.digits, hash)
		expected := sha256TestData[index]
		if token != expected {
			t.Errorf("SHA256 test data, token mismatch. Got %s, expected %s\n", token, expected)
		}
	}

	// Test SHA512
	otp.key = keySha512
	for index, ts := range timeCounters {
		counter := increment(ts, 30)
		otp.counter = bigEndianUint64(counter)
		hash := hmac.New(sha512.New, otp.key)
		token := calculateToken(otp.counter[:], otp.digits, hash)
		expected := sha512TestData[index]
		if token != expected {
			t.Errorf("SHA512 test data, token mismatch. Got %s, expected %s\n", token, expected)
		}
	}

}

func TestVerificationFailures(t *testing.T) {

	otp, err := NewTOTP("info@sec51.com", "Sec51", crypto.SHA1, 7)
	//checkError(t, err)
	if err != nil {
		t.Fatal(err)
	}

	// generate a new token
	expectedToken := otp.OTP()

	//verify the new token
	if err := otp.Validate(expectedToken); err != nil {
		t.Fatal(err)
	}

	// verify the wrong token for 3 times and check the internal counters values
	for i := 0; i < 3; i++ {
		if err := otp.Validate("1234567"); err == nil {
			t.Fatal(err)
		}
	}

	if otp.totalVerificationFailures != 3 {
		t.Errorf("Expected 3 verifcation failures, instead we've got %d\n", otp.totalVerificationFailures)
	}

	// at this point we crossed the max failures, therefore it should always return an error
	for i := 0; i < 10; i++ {
		if err := otp.Validate(expectedToken); err == nil {
			t.Fatal(err)
		}
	}

	// set the lastVerificationTime ahead in the future.
	// it should at this point pass
	back10Minutes := time.Duration(-10) * time.Minute
	otp.lastVerificationTime = time.Now().UTC().Add(back10Minutes)

	for i := 0; i < 10; i++ {
		if err := otp.Validate(expectedToken); err != nil {
			t.Fatal(err)
		}

		if i == 0 {
			// at this point the max failure counter should have been reset to zero
			if otp.totalVerificationFailures != 0 {
				t.Errorf("totalVerificationFailures counter not reset to zero. We've got: %d\n", otp.totalVerificationFailures)
			}
		}
	}

}

func TestIncrementCounter(t *testing.T) {

	ts := int64(1438601387)
	unixTime := time.Unix(ts, 0).UTC()
	// DEBUG
	// fmt.Println(time.Unix(ts, 0).UTC().Format(time.RFC1123))
	result := increment(unixTime.Unix(), 30)
	expected := uint64(47953379)
	if result != expected {
		t.Fatal("Error incrementing counter")
	}

}

func TestSerialization(t *testing.T) {
	// create a new TOTP
	otp, err := NewTOTP("info@sec51.com", "Sec51", crypto.SHA512, 8)
	if err != nil {
		t.Fatal(err)
	}

	// set some properties to a value different than the default
	otp.totalVerificationFailures = 2
	otp.stepSize = 27
	otp.lastVerificationTime = time.Now().UTC()
	otp.clientOffset = 1

	// Serialize it to bytes
	otpData, err := otp.ToBytes()
	if err != nil {
		t.Fatal(err)
	}

	// Convert it back from bytes to TOTP
	deserializedOTP, err := TOTPFromBytes(otpData)
	if err != nil {
		t.Fatal(err)
	}

	deserializedOTPData, err := deserializedOTP.ToBytes()
	if err != nil {
		t.Fatal(err)
	}

	if deserializedOTP == nil {
		t.Error("Could not deserialize back the TOTP object from bytes")
	}

	if bytes.Compare(deserializedOTP.key, otp.key) != 0 {
		t.Error("Deserialized digits property differ from original TOTP")
	}

	if deserializedOTP.digits != otp.digits {
		t.Error("Deserialized digits property differ from original TOTP")
	}

	if deserializedOTP.totalVerificationFailures != otp.totalVerificationFailures {
		t.Error("Deserialized totalVerificationFailures property differ from original TOTP")
	}

	if deserializedOTP.stepSize != otp.stepSize {
		t.Error("Deserialized stepSize property differ from original TOTP")
	}

	if deserializedOTP.lastVerificationTime.Unix() != otp.lastVerificationTime.Unix() {
		t.Error("Deserialized lastVerificationTime property differ from original TOTP")
	}

	if deserializedOTP.getIntCounter() != otp.getIntCounter() {
		t.Error("Deserialized counter property differ from original TOTP")
	}

	if deserializedOTP.clientOffset != otp.clientOffset {
		t.Error("Deserialized clientOffset property differ from original TOTP")
	}

	if deserializedOTP.account != otp.account {
		t.Error("Deserialized account property differ from original TOTP")
	}

	if deserializedOTP.issuer != otp.issuer {
		t.Error("Deserialized issuer property differ from original TOTP")
	}

	if deserializedOTP.OTP() != otp.OTP() {
		t.Error("Deserialized OTP token property differ from original TOTP")
	}

	if deserializedOTP.hashFunction != otp.hashFunction {
		t.Error("Deserialized hash property differ from original TOTP")
	}

	if deserializedOTP.URL() != otp.URL() {
		t.Error("Deserialized URL property differ from original TOTP")
	}

	if deserializedOTP.label() != otp.label() {
		t.Error("Deserialized Label property differ from original TOTP")
	}

	if base64.StdEncoding.EncodeToString(otpData) != base64.StdEncoding.EncodeToString(deserializedOTPData) {
		t.Error("Problems encoding TOTP to base64")
	}

	label, err := url.QueryUnescape(otp.label())
	if err != nil {
		t.Fatal(err)
	}

	if label != "Sec51:info@sec51.com" {
		t.Error("Creation of TOTP Label failed")
	}

}