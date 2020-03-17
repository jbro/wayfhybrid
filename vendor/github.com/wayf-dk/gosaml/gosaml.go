// Gosaml is a library for doing SAML stuff in Go.

package gosaml

import (
	"bytes"
	"compress/flate"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"hash"
	"html/template"
	"io"
	"io/ioutil"
	"log"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/wayf-dk/go-libxml2/types"
	"github.com/wayf-dk/goxml"
	"github.com/y0ssar1an/q"
)

var (
	_ = log.Printf // For debugging; delete when done.
	_ = q.Q
)

const (
	// IDPRole used to set the role as an IDP
	IDPRole = iota
	// SPRole used to set the role as an SP
	SPRole
)

const (
	// SAMLSign for SAML signing
	SAMLSign = iota
	// WSFedSign for WS-Fed signing
	WSFedSign
)

const (
	// XsDateTime Setting the Date Time
	XsDateTime = "2006-01-02T15:04:05Z"
	// SigningCertQuery refers to get the key from the metadata
	SigningCertQuery = `/md:KeyDescriptor[@use="signing" or not(@use)]/ds:KeyInfo/ds:X509Data/ds:X509Certificate`
	// EncryptionCertQuery refers to encryption key
	EncryptionCertQuery = `/md:KeyDescriptor[@use="encryption" or not(@use)]/ds:KeyInfo/ds:X509Data/ds:X509Certificate`
	// Transient refers to nameid format
	Transient = "urn:oasis:names:tc:SAML:2.0:nameid-format:transient"
	// Persistent refers to nameid format
	Persistent = "urn:oasis:names:tc:SAML:2.0:nameid-format:persistent"
	// X509 refers to nameid format
	X509 = "urn:oasis:names:tc:SAML:1.1:nameid-format:X509SubjectName"
	// Email refers to nameid format
	Email = "urn:oasis:names:tc:SAML:1.1:nameid-format:emailAddress"
	// Unspecified refers to unspecified nameid format
	Unspecified = "urn:oasis:names:tc:SAML:1.1:nameid-format:unspecified"

	// REDIRECT refers to HTTP-Redirect
	REDIRECT = "urn:oasis:names:tc:SAML:2.0:bindings:HTTP-Redirect"
	// POST refers to HTTP-POST
	POST = "urn:oasis:names:tc:SAML:2.0:bindings:HTTP-POST"
	// SIMPLESIGN refers to HTTP-POST-SimpleSign
	SIMPLESIGN = "urn:oasis:names:tc:SAML:2.0:bindings:HTTP-POST-SimpleSign"
	// Allowed slack for timingchecks
	timeskew = 90
	// SRequestPrefixLength - ardcoded prefix for special space saving encoding of sRequests
	SRequestPrefixLength = 5
)

type (
	// SamlRequest - compact representation of a request across the hub
	SamlRequest struct {
		Nonce, RequestID, SP, VirtualIDPID, AssertionConsumerIndex, Protocol string
		NameIDFormat, SPIndex, HubBirkIndex                                  uint8
	}

	// Md Interface for metadata provider
	Md interface {
		MDQ(key string) (xp *goxml.Xp, err error)
	}

	// MdSets slice of Md sets - for searching one MD at at time and remembering the index
	MdSets []Md

	// Conf refers to Configuration values for Schema and Certificates
	Conf struct {
		SamlSchema string
		CertPath   string
		LogPath    string
	}
	// SLOInfo refers to Single Logout information
	SLOInfo struct {
		IssuerID, NameID, SPNameQualifier, SessionIndex, DestinationID string
		NameIDFormat                                                   uint8
	}

	// Formdata for passing parameters to display template
	Formdata struct {
		Acs                       string
		Samlresponse, Samlrequest string
		RelayState                string
		WsFed                     bool
		Ard                       template.JS
	}

	xmapElement struct {
		key, xpath string
	}

	// Hm - HMac struct
	Hm struct {
		TTL  int64
		Hash func() hash.Hash
		Key  []byte
	}
)

var (
	// TestTime refers to global testing time
	TestTime time.Time
	// TestID for testing
	TestID string
	// TestAssertionID for testing
	TestAssertionID string
	// Roles refers to defining roles for SPs and IDPs
	Roles = []string{"md:IDPSSODescriptor", "md:SPSSODescriptor"}
	// Config initialisation
	Config = Conf{}
	// ErrorACS refers error information
	ErrorACS = errors.New("invalid AsssertionConsumerService or AsssertionConsumerServiceIndex")
	// NameIDList list of supported nameid formats
	NameIDList = []string{"", Transient, Persistent, X509, Email, Unspecified}
	// NameIDMap refers to mapping the nameid formats
	NameIDMap  = map[string]uint8{"": 1, Transient: 1, Persistent: 2, X509: 3, Email: 4, Unspecified: 5} // Unspecified accepted but not sent upstream
	whitespace = regexp.MustCompile("\\s")
	// PostForm -
	PostForm *template.Template
	// AuthnRequestCookie - shortlived hmaced timelimited data
	AuthnRequestCookie *Hm
)

// DebugSetting for debugging cookies
func DebugSetting(r *http.Request, name string) string {
	cookie, err := r.Cookie("debug")
	if err == nil {
		vals, _ := url.ParseQuery(cookie.Value)
		return vals.Get(name)
	}
	return ""
}

// DumpFile is for logging requests and responses
func DumpFile(r *http.Request, xp *goxml.Xp) (logtag string) {
	msgType := xp.QueryString(nil, "local-name(/*)")
	logtag = dump(msgType, []byte(fmt.Sprintf("%s\n###\n%s", xp.PP(), goxml.NewWerror("").Stack(1))))
	return
}

// DumpFileIfTracing - check trace flag and and dump if set
func DumpFileIfTracing(r *http.Request, xp *goxml.Xp) (logtag string) {
	if DebugSetting(r, "trace") == "1" {
		logtag = DumpFile(r, xp)
	}
	return
}

func dump(msgType string, blob []byte) (logtag string) {
	now := TestTime
	if now.IsZero() {
		now = time.Now()
	}
	logtag = now.Format("2006-01-02T15:04:05.0000000") // local time with microseconds
	if err := ioutil.WriteFile(fmt.Sprintf("log/%s-%s", logtag, msgType), blob, 0644); err != nil {
		//log.Panic(err)
	}
	return
}

// PublicKeyInfo extracts the keyname, publickey and cert (base64 DER - no PEM) from the given certificate.
// The keyname is computed from the public key corresponding to running this command: openssl x509 -modulus -noout -in <cert> | openssl sha1.
func PublicKeyInfo(cert string) (keyname string, publickey *rsa.PublicKey, err error) {
	// no pem so no pem.Decode
	key, err := base64.StdEncoding.DecodeString(whitespace.ReplaceAllString(cert, ""))
	pk, err := x509.ParseCertificate(key)
	if err != nil {
		return
	}
	publickey = pk.PublicKey.(*rsa.PublicKey)
	keyname = fmt.Sprintf("%x", sha1.Sum([]byte(fmt.Sprintf("Modulus=%X\n", publickey.N))))
	return
}

// GetPrivateKey extract the key from Metadata and builds a name and reads the key
func GetPrivateKey(md *goxml.Xp) (privatekey []byte, cert string, err error) {
	cert = md.Query1(nil, "./"+SigningCertQuery) // actual signing key is always first
	PP(cert)
	keyname, _, err := PublicKeyInfo(cert)
	if err != nil {
		return
	}

	privatekey, err = ioutil.ReadFile(Config.CertPath + keyname + ".key")
	if err != nil {
		return
	}
	return
}

// ID makes a random id
func ID() (id string) {
	b := make([]byte, 21) // 168 bits - just over the 160 bit recomendation without base64 padding
	rand.Read(b)
	return "_" + base64.RawURLEncoding.EncodeToString(b)
}

// IDHash to create hash of the id
func IDHash(data string) string {
	return fmt.Sprintf("%.5x", sha1.Sum([]byte(data)))
}

// Deflate utility that compresses a string using the flate algo
func Deflate(inflated []byte) []byte {
	var b bytes.Buffer
	w, _ := flate.NewWriter(&b, -1)
	w.Write(inflated)
	w.Close()
	return b.Bytes()
}

// Inflate utility that decompresses a string using the flate algo
func Inflate(deflated []byte) []byte {
	var b bytes.Buffer
	r := flate.NewReader(bytes.NewReader(deflated))
	b.ReadFrom(r)
	r.Close()
	return b.Bytes()
}

// HTML2SAMLResponse extracts the SAMLResponse from a HTML document
func HTML2SAMLResponse(html []byte) (samlresponse *goxml.Xp, relayState string) {
	response := goxml.NewHTMLXp(html)
	samlbase64 := response.Query1(nil, `//input[@name="SAMLResponse"]/@value`)
	relayState = response.Query1(nil, `//input[@name="RelayState"]/@value`)
	samlxml, _ := base64.StdEncoding.DecodeString(samlbase64)
	samlresponse = goxml.NewXp(samlxml)
	return
}

// URL2SAMLRequest extracts the SAMLRequest from an URL
func URL2SAMLRequest(url *url.URL, err error) (samlrequest *goxml.Xp, relayState string) {
	query := url.Query()
	req, _ := base64.StdEncoding.DecodeString(query.Get("SAMLRequest"))
	relayState = query.Get("RelayState")
	samlrequest = goxml.NewXp(Inflate(req))
	return
}

// SAMLRequest2URL creates a redirect URL from a saml request
func SAMLRequest2URL(samlrequest *goxml.Xp, relayState, privatekey, pw, algo string) (destination *url.URL, err error) {
	var paramName string
	switch samlrequest.QueryString(nil, "local-name(/*)") {
	case "LogoutResponse":
		paramName = "SAMLResponse="
	default:
		paramName = "SAMLRequest="
	}

	req := base64.StdEncoding.EncodeToString(Deflate(samlrequest.Dump()))

	destination, _ = url.Parse(samlrequest.Query1(nil, "@Destination"))
	q := paramName + url.QueryEscape(req)
	if relayState != "" {
		q += "&RelayState=" + url.QueryEscape(relayState)
	}

	if privatekey != "" {
		q += "&SigAlg=" + url.QueryEscape(goxml.Algos[algo].Signature)

		digest := goxml.Hash(goxml.Algos[algo].Algo, q)

		var signaturevalue []byte
		signaturevalue, err = goxml.Sign(digest, []byte(privatekey), []byte(pw), algo)
		if err != nil {
			return
		}
		signatureval := base64.StdEncoding.EncodeToString(signaturevalue)
		q += "&Signature=" + url.QueryEscape(signatureval)
	}

	destination.RawQuery = q
	return
}

// AttributeCanonicalDump for canonical dump
func AttributeCanonicalDump(w io.Writer, xp *goxml.Xp) {
	attrsmap := map[string][]string{}
	keys := []string{}
	attrs := xp.Query(nil, "./saml:Assertion/saml:AttributeStatement/saml:Attribute")
	for _, attr := range attrs {
		values := []string{}
		for _, value := range xp.QueryMulti(attr, "saml:AttributeValue") {
			values = append(values, value)
		}
		name := xp.Query1(attr, "@Name") + " "
		friendlyName := xp.Query1(attr, "@FriendlyName") + " "
		nameFormat := xp.Query1(attr, "@NameFormat")
		if name == friendlyName {
			friendlyName = ""
		}
		key := strings.TrimSpace(friendlyName + name + nameFormat)
		keys = append(keys, key)
		attrsmap[key] = values
	}

	sort.Strings(keys)
	for _, key := range keys {
		fmt.Fprintln(w, key)
		values := attrsmap[key]
		sort.Strings(values)
		for _, value := range values {
			if value != "" {
				fmt.Fprint(w, "    ")
				xml.EscapeText(w, bytes.TrimSpace([]byte(value)))
			}
			fmt.Fprintln(w)
		}
	}
}

// ReceiveAuthnRequest receives the authentication request
// Checks for Subject and  NameidPolicy(Persistent or Transient)
// Receives the metadatasets for resp. the sender and the receiver
// Returns metadata for the sender and the receiver
func ReceiveAuthnRequest(r *http.Request, issuerMdSets, destinationMdSets MdSets, location string) (xp, issuerMd, destinationMd *goxml.Xp, relayState string, issuerIndex, destinationIndex uint8, err error) {
	xp, issuerMd, destinationMd, relayState, issuerIndex, destinationIndex, err = DecodeSAMLMsg(r, issuerMdSets, destinationMdSets, IDPRole, []string{"AuthnRequest"}, location, nil)
	if err != nil {
		return
	}
	subject := xp.Query1(nil, "./saml:Subject")
	if subject != "" {
		err = fmt.Errorf("subject not allowed in SAMLRequest")
		return
	}
	nameIDFormat := xp.Query1(nil, "./samlp:NameIDPolicy/@Format")
	if NameIDMap[nameIDFormat] == 0 {
		err = fmt.Errorf("nameidpolicy format: '%s' is not supported", nameIDFormat)
		return
	}

	if nameIDFormat == Transient {
	} else if nameIDFormat == Unspecified || nameIDFormat == "" {
		nameIDFormat = issuerMd.Query1(nil, "./md:SPSSODescriptor/md:NameIDFormat") // none ends up being Transient
	} else if inArray(nameIDFormat, issuerMd.QueryMulti(nil, "./md:SPSSODescriptor/md:NameIDFormat")) {
	} else {
		nameIDFormat = Transient
	}
	xp.QueryDashP(nil, "./samlp:NameIDPolicy/@Format", nameIDFormat, nil)

	/*
		allowcreate := xp.Query1(nil, "./samlp:NameIDPolicy/@AllowCreate")
		if allowcreate != "true" && allowcreate != "1" {
			err = fmt.Errorf("only supported value for NameIDPolicy @AllowCreate is true/1, got: %s", allowcreate)
			return
		}
	*/
	return
}

func inArray(item string, array []string) bool {
	for _, i := range array {
		if i == item {
			return true
		}
	}
	return false
}

// FindInMetadataSets - find an entity in a list of MD sets and return it and the index
func FindInMetadataSets(metadataSets MdSets, key string) (md *goxml.Xp, index uint8, err error) {
	for i := range metadataSets {
		index = uint8(i)
		md, err = metadataSets[index].MDQ(key)
		if err == nil { // if we don't get md not found the last error is as good as the first
			return
		}
	}
	return
}

// ReceiveSAMLResponse handles the SAML minutiae when receiving a SAMLResponse
// Currently the only supported binding is POST
// Receives the metadatasets for resp. the sender and the receiver
// Returns metadata for the sender and the receiver
func ReceiveSAMLResponse(r *http.Request, issuerMdSets, destinationMdSets MdSets, location string, xtraCerts []string) (xp, issuerMd, destinationMd *goxml.Xp, relayState string, issuerIndex, destinationIndex uint8, err error) {
	return DecodeSAMLMsg(r, issuerMdSets, destinationMdSets, SPRole, []string{"Response"}, location, xtraCerts)
}

// ReceiveLogoutMessage receives the Logout Message
// Receives the metadatasets for resp. the sender and the receiver
// Returns metadata for the sender and the receiver
func ReceiveLogoutMessage(r *http.Request, issuerMdSets, destinationMdSets MdSets, role int) (xp, issuerMd, destinationMd *goxml.Xp, relayState string, issuerIndex, destinationIndex uint8, err error) {
	return DecodeSAMLMsg(r, issuerMdSets, destinationMdSets, role, []string{"LogoutRequest", "LogoutResponse"}, "https://"+r.Host+r.URL.Path, nil)
}

// DecodeSAMLMsg decodes the Request. Extracts Issuer, Destination
// Check for Protocol for example (AuthnRequest)
// Validates the schema
// Receives the metadatasets for resp. the sender and the receiver
// Returns metadata for the sender and the receiver
func DecodeSAMLMsg(r *http.Request, issuerMdSets, destinationMdSets MdSets, role int, protocols []string, location string, xtraCerts []string) (xp, issuerMd, destinationMd *goxml.Xp, relayState string, issuerIndex, destinationIndex uint8, err error) {
	defer r.Body.Close()
	r.ParseForm()
	method := r.Method

	if ok := method == "GET" || method == "POST"; !ok {
		err = fmt.Errorf("unsupported http method used '%s'", method)
		return
	}

	relayState = r.Form.Get("RelayState")

	msg := r.Form.Get("SAMLRequest")
	if msg == "" {
		msg = r.Form.Get("SAMLResponse")
		if msg == "" {
			msg, relayState, err = request2samlRequest(r, issuerMdSets, destinationMdSets)
			if err != nil {
				return
			}
		}
	}

	bmsg, err := base64.StdEncoding.DecodeString(msg)
	if err != nil {
		return
	}
	if method == "GET" {
		bmsg = Inflate(bmsg)
	}

	tmpXp := goxml.NewXp(bmsg)

	DumpFileIfTracing(r, tmpXp)
	//log.Println("stack", goxml.New().Stack(1))
	errs, err := tmpXp.SchemaValidate(Config.SamlSchema)
	if err != nil {
		dump("raw", bmsg)
		err = goxml.Wrap(err)
		fmt.Println(errs)
		return
	}

	protocol := tmpXp.QueryString(nil, "local-name(/*)")
	var protocolOK bool
	for _, expectedProtocol := range protocols {
		protocolOK = protocolOK || protocol == expectedProtocol
	}

	if !protocolOK {
		err = fmt.Errorf("expected protocol(s) %v not found, got %s", protocols, protocol)
		return
	}

	issuer := tmpXp.Query1(nil, "./saml:Issuer")
	if issuer == "" {
		err = fmt.Errorf("no issuer found in SAMLRequest/SAMLResponse")
		return
	}

	issuerMd, issuerIndex, err = FindInMetadataSets(issuerMdSets, issuer)
	if err != nil {
		return
	}

	key := location
	// we only receive responses for requests we have made ourselves - either from an IdP or via SAML2jwt, i.e. we have an encoded SamlRequest in @InResponseTo
	if protocol == "Response" {
		var tmpID []byte
		tmpID, err = AuthnRequestCookie.SpcDecode("id", tmpXp.Query1(nil, "./@InResponseTo")[1:], SRequestPrefixLength) // skip _
		if err != nil {
			return
		}
		sRequest := SamlRequest{}
		sRequest.Unmarshal(tmpID)
		if sRequest.RequestID == "" { // An "edge" request - i.e. not across the hub
			key = sRequest.SP
		}
	}

	destination := tmpXp.Query1(nil, "./@Destination")
	if destination == "" {
		err = fmt.Errorf("no destination found in SAMLRequest/SAMLResponse")
		return
	}

	destinationMd, destinationIndex, err = FindInMetadataSets(destinationMdSets, key)
	if err != nil {
		return
	}

	if destination != location && !strings.HasPrefix(destination, location+"?") { // ignore params ...
		err = fmt.Errorf("destination: %s is not here, here is %s", destination, location)
		return
	}

	xp, err = CheckSAMLMessage(r, tmpXp, issuerMd, destinationMd, role, destination, xtraCerts)
	if err != nil {
		err = goxml.Wrap(err)
		return
	}

	xp, err = checkDestinationAndACS(xp, issuerMd, destinationMd, role, destination)
	if err != nil {
		return
	}

	xp, err = VerifyTiming(xp)
	if err != nil {
		return
	}
	return
}

// CheckSAMLMessage checks for Authentication Requests, Reponses and Logout Requests
// Checks for invalid Bindings. Check for Certificates. Verify Signatures
func CheckSAMLMessage(r *http.Request, xp, issuerMd, destinationMd *goxml.Xp, role int, location string, xtraCerts []string) (validatedMessage *goxml.Xp, err error) {
	type protoCheckInfoStruct struct {
		minSignatures     int
		service           string
		signatureElements []string
		checks            []string
	}
	// add checks for xtra element on top level in tests - does schema checks handle that or should we do it here???
	protoChecks := map[string]protoCheckInfoStruct{
		"AuthnRequest": {
			minSignatures:     map[bool]int{true: 1, false: 0}[destinationMd.QueryXMLBool(nil, "./md:IDPSSODescriptor/@WantAuthnRequestsSigned")],
			service:           "md:SingleSignOnService",
			signatureElements: []string{"/samlp:AuthnRequest[1]/ds:Signature[1]/..]", ""}},
		"Response": {
			minSignatures:     1,
			service:           "md:AssertionConsumerService",
			signatureElements: []string{"/samlp:Response[1]/ds:Signature[1]/..", "/samlp:Response[1]/saml:Assertion[1]/ds:Signature[1]/.."},
			checks:            []string{"count(/samlp:Response/saml:Assertion) = 1", "/samlp:Response/saml:Issuer = /samlp:Response/saml:Assertion/saml:Issuer"}},
		"LogoutRequest": {
			minSignatures:     0,
			service:           "md:SingleLogoutService",
			signatureElements: []string{"/samlp:LogoutRequest[1]/ds:Signature[1]/..", ""}},
		"LogoutResponse": {
			minSignatures:     0,
			service:           "md:SingleLogoutService",
			signatureElements: []string{"/samlp:LogoutResponse[1]/ds:Signature[1]/..", ""}},
	}

	protocol := xp.QueryString(nil, "local-name(/*)")

	bindings := map[string][]string{
		"GET":  {REDIRECT},
		"POST": {POST, SIMPLESIGN},
	}

	var usedBinding string
	validBinding := false

findbinding:
	for _, usedBinding = range bindings[r.Method] {
		for _, v := range destinationMd.QueryMulti(nil, `./`+Roles[role]+`/`+protoChecks[protocol].service+`[@Location=`+strconv.Quote(location)+`]/@Binding`) {
			validBinding = v == usedBinding
			if validBinding {
				break findbinding
			}
		}
	}

	if !validBinding || usedBinding == "" {
		err = errors.New("No valid binding found in metadata")
		return
	}

	if protoChecks[protocol].minSignatures <= 0 {
		return xp, nil
	}

	certificates := issuerMd.QueryMulti(nil, `./`+Roles[(role+1)%2]+SigningCertQuery) // the issuer's role
	certificates = append(certificates, xtraCerts...)

	if len(certificates) == 0 {
		err = errors.New("no certificates found in metadata")
		return
	}

	if usedBinding == REDIRECT {
		if _, ok := r.Form["SigAlg"]; !ok && protoChecks[protocol].minSignatures <= 0 {
			return xp, nil
		}
		rawValues := parseQueryRaw(r.URL.RawQuery)
		query := ""
		delim := ""
		for _, key := range []string{"SAMLRequest", "SAMLResponse", "RelayState", "SigAlg"} {
			if rw, ok := rawValues[key]; ok {
				query += delim + key + "=" + rw[0]
				delim = "&"
			}
		}

		sigAlg := r.Form.Get("SigAlg") // needed as decoded value
		if _, ok := goxml.Algos[sigAlg]; !ok {
			return nil, goxml.NewWerror("unsupported SigAlg", sigAlg)
		}
		digest := goxml.Hash(goxml.Algos[sigAlg].Algo, query)
		signature, _ := base64.StdEncoding.DecodeString(r.Form.Get("Signature"))
		verified := 0
		signerrors := []error{}
		for _, certificate := range certificates {
			var pub *rsa.PublicKey
			_, pub, err = PublicKeyInfo(certificate)

			if err != nil {
				return nil, goxml.Wrap(err)
			}
			signerror := rsa.VerifyPKCS1v15(pub, goxml.Algos[sigAlg].Algo, digest[:], signature)
			if signerror != nil {
				signerrors = append(signerrors, signerror)
			} else {
				verified++
				break
			}
		}
		if verified != 1 {
			errorstring := ""
			delim := ""
			for _, e := range signerrors {
				errorstring += e.Error() + delim
				delim = ", "
			}
			err = goxml.NewWerror("cause:unable to validate signature", errorstring)
			return
		}
		validatedMessage = xp
	}

	if usedBinding == POST {
		if query := protoChecks[protocol].signatureElements[0]; query != "" {
			signatures := xp.Query(nil, query)
			if len(signatures) == 1 {
				if err = VerifySign(xp, certificates, signatures[0]); err != nil {
					return
				}
				validatedMessage = xp
			}
		}
		if protocol == "Response" {
			encryptedAssertions := xp.Query(nil, "/samlp:Response/saml:EncryptedAssertion")
			if len(encryptedAssertions) == 1 {

				cert := destinationMd.Query1(nil, "./md:SPSSODescriptor"+EncryptionCertQuery) // actual encryption key is always first
				var keyname string
				keyname, _, err = PublicKeyInfo(cert)
				if err != nil {
					return nil, goxml.Wrap(err)
				}
				var privatekey []byte

				privatekey, err = ioutil.ReadFile(Config.CertPath + keyname + ".key")
				if err != nil {
					return nil, goxml.Wrap(err)
				}

				encryptedAssertion := encryptedAssertions[0]
				encryptedData := xp.Query(encryptedAssertion, "xenc:EncryptedData")[0]
				decryptedAssertion, err := xp.Decrypt(encryptedData.(types.Element), privatekey, []byte("-"))
				if err != nil {
					err = goxml.Wrap(err)
					err = goxml.PublicError(err.(goxml.Werror), "cause:encryption error") // hide the real problem from attacker
					return nil, err
				}

				decryptedAssertionElement, _ := decryptedAssertion.Doc.DocumentElement()
				decryptedAssertionElement = xp.CopyNode(decryptedAssertionElement, 1)
				_ = encryptedAssertion.AddPrevSibling(decryptedAssertionElement)
				goxml.RmElement(encryptedAssertion)

				// repeat schemacheck
				_, err = xp.SchemaValidate(Config.SamlSchema)
				if err != nil {
					err = goxml.Wrap(err)
					err = goxml.PublicError(err.(goxml.Werror), "cause:encryption error") // hide the real problem from attacker
					return nil, err
				}
			} else if len(encryptedAssertions) != 0 {
				err = fmt.Errorf("only 1 EncryptedAssertion allowed, %d found", len(encryptedAssertions))
			}
		}
		// Only Responses with an Assertion will have a second signatureElements query
		if query := protoChecks[protocol].signatureElements[1]; query != "" {
			signatures := xp.Query(nil, query)
			if len(signatures) == 1 {
				if err = VerifySign(xp, certificates, signatures[0]); err != nil {
					return nil, goxml.Wrap(err, "err:unable to validate signature")
				}
				//validatedMessage = xp
				// we trust the whole message if the first signature was validated

				if validatedMessage == nil {
					// replace with the validated assertion
					validatedMessage = goxml.NewXp(nil)
					shallowresponse := validatedMessage.CopyNode(xp.Query(nil, "/samlp:Response[1]")[0], 2)
					validatedMessage.Doc.SetDocumentElement(shallowresponse)
					validatedMessage.QueryDashP(nil, "./saml:Issuer", xp.Query1(nil, "/samlp:Response/saml:Issuer"), nil)
					validatedMessage.QueryDashP(nil, "./samlp:Status/samlp:StatusCode/@Value", xp.Query1(nil, "/samlp:Response/samlp:Status/samlp:StatusCode/@Value"), nil)
					shallowresponse.AddChild(validatedMessage.CopyNode(xp.Query(nil, "/samlp:Response[1]/saml:Assertion[1]")[0], 1))
				}
			}
		}
	}

	if usedBinding == SIMPLESIGN {
		return nil, goxml.NewWerror("err:SimpleSign not yet supported")
	}

	// if we don't have a validatedResponse by now we are toast
	if validatedMessage == nil {
		err = goxml.NewWerror("err:no signatures found")
		err = goxml.PublicError(err.(goxml.Werror), "cause:encryption error") // hide the real problem from attacker
		return nil, err
	}

	for _, check := range protoChecks[protocol].checks {
		if !validatedMessage.QueryBool(nil, check) {
			return nil, goxml.NewWerror("cause: check failed", "check: "+check)
		}
	}
	return
}

// checkDestinationAndACS checks for valid destination
// Returns Error Otherwise
func checkDestinationAndACS(message, issuerMd, destinationMd *goxml.Xp, role int, location string) (checkedMessage *goxml.Xp, err error) {
	var checkedDest string
	var acsIndex string
	mdRole := "./" + Roles[role]
	protocol := message.QueryString(nil, "local-name(/*)")
	switch protocol {
	case "AuthnRequest":
		acs := message.Query1(nil, "@AssertionConsumerServiceURL")
		if acs == "" {
			acsIndex := message.Query1(nil, "@AttributeConsumingServiceIndex")
			acs = issuerMd.Query1(nil, `./md:SPSSODescriptor/md:AssertionConsumerService[@Index=`+strconv.Quote(acsIndex)+`]/@Location`)
		}
		if acs == "" {
			acs = issuerMd.Query1(nil, `./md:SPSSODescriptor/md:AssertionConsumerService[@Binding="`+POST+`" and (@isDefault="true" or @isDefault!="false" or not(@isDefault))]/@Location`)
		}

		checkedAcs := issuerMd.Query1(nil, `./md:SPSSODescriptor/md:AssertionConsumerService[@Binding="`+POST+`" and @Location=`+strconv.Quote(acs)+`]/@index`)
		if checkedAcs == "" {
			return nil, goxml.Wrap(ErrorACS, "acs:"+acs, "acsindex:"+acsIndex)
		}

		// we now have a validated AssertionConsumerService - and Binding - let's put them into the request
		message.QueryDashP(nil, "@AssertionConsumerServiceURL", acs, nil)
		message.QueryDashP(nil, "@ProtocolBinding", POST, nil)
		message.QueryDashP(nil, "@AssertionConsumerServiceIndex", checkedAcs, nil)

		checkedDest = destinationMd.Query1(nil, `./md:IDPSSODescriptor/md:SingleSignOnService[@Binding="`+REDIRECT+`" and @Location=`+strconv.Quote(location)+`]/@Location`)
		if checkedDest == "" {
			checkedDest = destinationMd.Query1(nil, `./md:IDPSSODescriptor/md:SingleSignOnService[@Binding="`+POST+`" and @Location=`+strconv.Quote(location)+`]/@Location`)
		}
	case "LogoutRequest", "LogoutResponse":
		checkedDest = destinationMd.Query1(nil, mdRole+`/md:SingleLogoutService[@Location=`+strconv.Quote(location)+`]/@Location`)
	case "Response":
		recipient := message.Query1(nil, "./saml:Assertion/saml:Subject/saml:SubjectConfirmation/saml:SubjectConfirmationData/@Recipient")

		if recipient == "" {
			err = fmt.Errorf("no receipient found in SubjectConfirmationData")
			return
		}

		if recipient != location {
			err = fmt.Errorf("response.Destination != SubjectConfirmationData.Recipient")
			return
		}

		issuer := message.Query1(nil, "./saml:Issuer") // already checked

		assertionIssuer := message.Query1(nil, "./saml:Assertion/saml:Issuer")
		if assertionIssuer == "" {
			err = fmt.Errorf("no issuer found in Assertion")
			return
		}

		if issuer != assertionIssuer {
			err = fmt.Errorf("response.Issuer != assertion.Issuer not supported")
			return
		}

		rInResponseTo := message.Query1(nil, "./@InResponseTo")
		aInResponseTo := message.Query1(nil, "./saml:Assertion/saml:Subject/saml:SubjectConfirmation/saml:SubjectConfirmationData/@InResponseTo")

		if rInResponseTo != aInResponseTo {
			return nil, goxml.NewWerror("cause:InResponseTo not the same in Response and Assertion")
		}
		checkedDest = destinationMd.Query1(nil, `./md:SPSSODescriptor/md:AssertionConsumerService[@Binding="`+POST+`" and @Location=`+strconv.Quote(location)+`]/@Location`)
	}
	if checkedDest == "" {
		return nil, goxml.NewWerror("Destination is not valid", "destination:"+location)
	}
	checkedMessage = message
	return
}

// parseQueryRaw from src/net/url/url.go - return raw query values - needed for checking signatures
func parseQueryRaw(query string) url.Values {
	m := make(url.Values)
	for query != "" {
		key := query
		if i := strings.IndexAny(key, "&"); i >= 0 {
			key, query = key[:i], key[i+1:]
		} else {
			query = ""
		}
		if key == "" {
			continue
		}
		value := ""
		if i := strings.Index(key, "="); i >= 0 {
			key, value = key[:i], key[i+1:]
		}
		m[key] = append(m[key], value)
	}
	return m
}

// VerifySign takes Certificate, signature and xp as an input
func VerifySign(xp *goxml.Xp, certificates []string, signature types.Node) (err error) {
	publicKeys := []*rsa.PublicKey{}
	for _, certificate := range certificates {
		var key *rsa.PublicKey
		_, key, err = PublicKeyInfo(certificate)
		if err != nil {
			return
		}
		publicKeys = append(publicKeys, key)
	}

	return xp.VerifySignature(signature, publicKeys)
}

// VerifyTiming verify the presence and value of timestamps
func VerifyTiming(xp *goxml.Xp) (verifiedXp *goxml.Xp, err error) {
	type timing struct {
		required     bool
		notonorafter bool
		notbefore    bool
	}

	now := TestTime
	if now.IsZero() {
		now = time.Now()
	}
	intervalstart := now.Add(-time.Duration(timeskew) * time.Second).UTC()
	intervalend := now.Add(time.Duration(timeskew) * time.Second).UTC()

	var checks map[string]timing

	protocol := xp.QueryString(nil, "local-name(/*)")
	switch protocol {
	case "AuthnRequest", "LogoutRequest", "LogoutResponse":
		checks = map[string]timing{
			"./@IssueInstant": {true, true, true},
		}
	case "Response":
		checks = map[string]timing{
			"/samlp:Response[1]/@IssueInstant": {true, true, true},
			//			"/samlp:Response[1]/saml:Assertion[1]/@IssueInstant":                                                                    timing{true, true, true},
			"/samlp:Response[1]/saml:Assertion[1]/saml:Subject/saml:SubjectConfirmation/saml:SubjectConfirmationData/@NotOnOrAfter": {false, true, false},
			"/samlp:Response[1]/saml:Assertion[1]/saml:Conditions/@NotBefore":                                                       {false, false, true},
			"/samlp:Response[1]/saml:Assertion[1]/saml:Conditions/@NotOnOrAfter":                                                    {false, true, false},
			//			"/samlp:Response[1]/saml:Assertion[1]/saml:AuthnStatement/@AuthnInstant":                                                timing{true, true, true},
			//			"/samlp:Response[1]/saml:Assertion[1]/saml:AuthnStatement/@SessionNotOnOrAfter":                                         timing{false, true, false},
		}
	}

	for q, t := range checks {
		xmltime := xp.Query1(nil, q)
		if t.required && xmltime == "" {
			err = fmt.Errorf("required timestamp: %s not present in: %s", q, protocol)
			return
		}
		if xmltime != "" {
			samltime, err := time.Parse(XsDateTime, xmltime)
			if err != nil {
				return nil, err
			}
			ok := true
			if t.notbefore {
				ok = ok && samltime.Before(intervalend)
			}
			if t.notonorafter {
				ok = ok && intervalstart.Before(samltime)
			}
			if !ok { // Only check if the time is actually there
				err = fmt.Errorf("timing problem: %s  %s < %s <= %s", q, intervalstart, samltime, intervalend)
				return nil, err
			}
		}
	}
	verifiedXp = xp
	return
}

// IDAndTiming for checking the validity
func IDAndTiming() (issueInstant, id, assertionID, assertionNotOnOrAfter, sessionNotOnOrAfter string) {
	now := TestTime
	if now.IsZero() {
		now = time.Now()
	}
	issueInstant = now.Format(XsDateTime)
	assertionNotOnOrAfter = now.Add(4 * time.Minute).Format(XsDateTime)
	sessionNotOnOrAfter = now.Add(4 * time.Hour).Format(XsDateTime)
	id = TestID
	if id == "" {
		id = ID()
	}
	assertionID = TestAssertionID
	if assertionID == "" {
		assertionID = ID()
	}
	return
}

// NewErrorResponse makes a new error response with Entityid, issuer, destination and returns the response
func NewErrorResponse(idpMd, spMd, authnrequest, sourceResponse *goxml.Xp) (response *goxml.Xp) {
	idpEntityID := idpMd.Query1(nil, `/md:EntityDescriptor/@entityID`)
	response = goxml.NewXpFromNode(sourceResponse.DocGetRootElement())
	response.QueryDashP(nil, "./@InResponseTo", authnrequest.Query1(nil, "@ID"), nil)
	response.QueryDashP(nil, "./@Destination", authnrequest.Query1(nil, "@AssertionConsumerServiceURL"), nil)
	response.QueryDashP(nil, "./saml:Issuer", idpEntityID, nil)
	response.Rm(nil, `./saml:Assertion`)
	return
}

// NewLogoutRequest makes a logout request with issuer destination ... and returns a NewRequest
func NewLogoutRequest(destination *goxml.Xp, sloinfo *SLOInfo, role int) (request *goxml.Xp, binding string, err error) {
	template := `<samlp:LogoutRequest xmlns:samlp="urn:oasis:names:tc:SAML:2.0:protocol" xmlns:saml="urn:oasis:names:tc:SAML:2.0:assertion" Version="2.0"></samlp:LogoutRequest>`
	request = goxml.NewXpFromString(template)
	issueInstant, _, _, _, _ := IDAndTiming()

	slo := destination.Query(nil, `./`+Roles[role]+`/md:SingleLogoutService[@Binding="`+REDIRECT+`" or @Binding="`+POST+`"]`)
	if len(slo) == 0 {
		err = goxml.NewWerror("cause:no SingleLogoutService found", "entityID:"+destination.Query1(nil, "./@entityID"))
		return
	}

	binding = destination.Query1(slo[0], "./@Binding")

	request.QueryDashP(nil, "./@IssueInstant", issueInstant, nil)
	request.QueryDashP(nil, "./@ID", ID(), nil)
	request.QueryDashP(nil, "./@Destination", destination.Query1(slo[0], "./@Location"), nil)
	request.QueryDashP(nil, "./saml:Issuer", sloinfo.IssuerID, nil)

	request.QueryDashP(nil, "./saml:NameID/@Format", NameIDList[sloinfo.NameIDFormat], nil)
	if sloinfo.SPNameQualifier != "" {
		request.QueryDashP(nil, "./saml:NameID/@SPNameQualifier", sloinfo.SPNameQualifier, nil)
	}
	if sloinfo.SessionIndex != "" {
		request.QueryDashP(nil, "./samlp:SessionIndex", sloinfo.SessionIndex, nil)
	}
	request.QueryDashP(nil, "./saml:NameID", sloinfo.NameID, nil)
	return
}

// NewLogoutResponse creates a Logout Response oon the basis of Logout request
func NewLogoutResponse(issuer, destination, request *goxml.Xp, role int) (response *goxml.Xp, binding string, err error) {
	template := `<samlp:LogoutResponse xmlns:samlp="urn:oasis:names:tc:SAML:2.0:protocol"
                      xmlns:saml="urn:oasis:names:tc:SAML:2.0:assertion"
                      ID=""
                      Version="2.0"
                      IssueInstant=""
                      Destination=""
                      InResponseTo="">
    <saml:Issuer>
     https://wayf.wayf.dk
    </saml:Issuer>
    <samlp:Status>
        <samlp:StatusCode Value="urn:oasis:names:tc:SAML:2.0:status:Success"/>
    </samlp:Status>
</samlp:LogoutResponse>
`
	response = goxml.NewXpFromString(template)
	slo := destination.Query(nil, `./`+Roles[role]+`/md:SingleLogoutService[@Binding="`+REDIRECT+`" or @Binding="`+POST+`"]`)
	if len(slo) == 0 {
		err = goxml.NewWerror("cause:no SingleLogoutService found", "entityID:"+destination.Query1(nil, "./@entityID"))
		return
	}
	binding = destination.Query1(slo[0], "./@Binding")
	response.QueryDashP(nil, "./@Destination", destination.Query1(slo[0], "./@Location"), nil)

	response.QueryDashP(nil, "./@IssueInstant", time.Now().Format(XsDateTime), nil)
	response.QueryDashP(nil, "./@ID", ID(), nil)
	response.QueryDashP(nil, "./@InResponseTo", request.Query1(nil, "./@ID"), nil)
	response.QueryDashP(nil, "./saml:Issuer", issuer.Query1(nil, `/md:EntityDescriptor/@entityID`), nil)
	return
}

// SloRequest generates a single logout request
func SloRequest(w http.ResponseWriter, r *http.Request, response, spMd, IdpMd *goxml.Xp, pk string) {
	sloinfo := NewSLOInfo(response, spMd.Query1(nil, "@entityID"))
	sloinfo.IssuerID = spMd.Query1(nil, "@entityID")
	request, binding, _ := NewLogoutRequest(IdpMd, sloinfo, IDPRole)
	switch binding {
	case REDIRECT:
		u, _ := SAMLRequest2URL(request, "", pk, "-", "")
		http.Redirect(w, r, u.String(), http.StatusFound)
	case POST:
		data := Formdata{Acs: request.Query1(nil, "./@Destination"), Samlrequest: base64.StdEncoding.EncodeToString(request.Dump())}
		PostForm.ExecuteTemplate(w, "postForm", data)
	}
}

// SloResponse generates a single logout reponse
func SloResponse(w http.ResponseWriter, r *http.Request, request, issuer, destination *goxml.Xp, pk string) {
	response, binding, _ := NewLogoutResponse(issuer, destination, request, IDPRole)
	switch binding {
	case REDIRECT:
		u, _ := SAMLRequest2URL(response, "", pk, "-", "")
		http.Redirect(w, r, u.String(), http.StatusFound)
	case POST:
		data := Formdata{Acs: response.Query1(nil, "./@Destination"), Samlresponse: base64.StdEncoding.EncodeToString(response.Dump())}
		PostForm.ExecuteTemplate(w, "postForm", data)
	}
}

// NewSLOInfo extract necessary Logout information
func NewSLOInfo(response *goxml.Xp, de string) (slo *SLOInfo) {
	slo = &SLOInfo{
		IssuerID:        response.Query1(nil, "/samlp:Response/saml:Assertion/saml:Issuer"),
		NameID:          response.Query1(nil, "/samlp:Response/saml:Assertion/saml:Subject/saml:NameID"),
		NameIDFormat:    NameIDMap[response.Query1(nil, "/samlp:Response/saml:Assertion/saml:Subject/saml:NameID/@Format")],
		SPNameQualifier: response.Query1(nil, "/samlp:Response/saml:Assertion/saml:Subject/saml:NameID/@SPNameQualifier"),
		SessionIndex:    response.Query1(nil, "/samlp:Response/saml:Assertion/saml:AuthnStatement/@SessionIndex"),
		DestinationID:   de,
	}
	return
}

// SignResponse signs the response with the given method.
// Returns an error if unable to sign.
func SignResponse(response *goxml.Xp, elementQuery string, md *goxml.Xp, signingMethod string, signFor int) (err error) {
	privatekey, cert, err := GetPrivateKey(md)
	if err != nil {
		return
	}

	element := response.Query(nil, elementQuery)
	if len(element) != 1 {
		err = errors.New("did not find exactly one element to sign")
		return
	}
	// Put signature before 2nd child - ie. after Issuer
	var before types.Node
	switch signFor {
	case SAMLSign:
		before = response.Query(element[0], "*[2]")[0]
	case WSFedSign:
		before = nil
	}

	err = response.Sign(element[0].(types.Element), before, privatekey, []byte("-"), cert, signingMethod)
	return
}

// NewAuthnRequest - create an AuthnRequest using the supplied metadata for setting the fields according to the following rules:
//  - The Destination is the 1st SingleSignOnService with a redirect binding in the idpmetadata
//  - The AssertionConsumerServiceURL is the Location of the 1st ACS with a post binding in the spmetadata
//  - The ProtocolBinding is post
//  - The Issuer is the entityID in the idpmetadata
//  - The NameID defaults to transient
func NewAuthnRequest(originalRequest, spMd, idpMd *goxml.Xp, virtualIDPID string, idPList []string, acs string, wantRequesterID bool, spIndex, hubBirkIndex uint8) (request *goxml.Xp, err error) {
	template := `<samlp:AuthnRequest xmlns:samlp="urn:oasis:names:tc:SAML:2.0:protocol"
                    xmlns:saml="urn:oasis:names:tc:SAML:2.0:assertion"
                    Version="2.0"
                    ProtocolBinding="urn:oasis:names:tc:SAML:2.0:bindings:HTTP-POST"
                    >
<saml:Issuer>Issuer</saml:Issuer>
<samlp:NameIDPolicy Format="urn:oasis:names:tc:SAML:2.0:nameid-format:transient" AllowCreate="true" />
</samlp:AuthnRequest>`
	issueInstant, msgID, _, _, _ := IDAndTiming()
	issuer := spMd.Query1(nil, `./@entityID`)
	ID := ""
	protocol := ""

	request = goxml.NewXpFromString(template)
	//request.QueryDashP(nil, "./@ID", msgID, nil)
	request.QueryDashP(nil, "./@IssueInstant", issueInstant, nil)
	request.QueryDashP(nil, "./@Destination", idpMd.Query1(nil, `./md:IDPSSODescriptor/md:SingleSignOnService[@Binding="`+REDIRECT+`"]/@Location`), nil)
	acses := spMd.QueryMulti(nil, `./md:SPSSODescriptor/md:AssertionConsumerService[@Binding="`+POST+`"]/@Location`)
	if acs == "" {
		acs = acses[0]
	}
	acsIndex := spMd.Query1(nil, `./md:SPSSODescriptor/md:AssertionConsumerService[@Binding="`+POST+`" and @Location=`+strconv.Quote(acs)+`]/@index`)
	request.QueryDashP(nil, "./@AssertionConsumerServiceURL", acs, nil)
	request.QueryDashP(nil, "./saml:Issuer", issuer, nil)
	for _, providerID := range idPList {
		if providerID != "" {
			request.QueryDashP(nil, "./samlp:Scoping/samlp:IDPList/samlp:IDPEntry[0]/@ProviderID", providerID, nil)
		}
	}
	nameIDFormat := ""
	nameIDFormats := NameIDList

	if originalRequest != nil { // already checked for supported nameidformat
		fmt.Println("origreq", originalRequest.PP())
		if originalRequest.QueryXMLBool(nil, "./@ForceAuthn") {
			request.QueryDashP(nil, "./@ForceAuthn", "true", nil)
		}
		if originalRequest.QueryXMLBool(nil, "./@IsPassive") {
			request.QueryDashP(nil, "./@IsPassive", "true", nil)
		}
		//requesterID := originalRequest.Query1(nil, "./saml:Issuer")
		//request.QueryDashP(nil, "./samlp:Scoping/samlp:RequesterID", requesterID, nil)
		if nameIDPolicy := originalRequest.Query1(nil, "./samlp:NameIDPolicy/@Format"); nameIDPolicy != "" {
			nameIDFormats = append([]string{nameIDPolicy}, nameIDFormats...) // prioritize what the SP asked for
		}
		issuer = originalRequest.Query1(nil, "./saml:Issuer")
		ID = originalRequest.Query1(nil, "./@ID")
		// var origID []byte
		// origID, err = AuthnRequestCookie.SpcDecode("id", ID[1:], SRequestPrefixLength) // skip _
		// if err == nil {
		// 	// need a field in SamlRequest for remembering ...
		// 	//			ID = string(origID[SRequestPrefixLength+1:]) // one of our own - save what can be saved
		// }
		nameIDFormat = originalRequest.Query1(nil, "./samlp:NameIDPolicy/@Format")
		protocol = originalRequest.Query1(nil, "./samlp:Extensions/wayf:protocol")
		acsIndex = originalRequest.Query1(nil, "./@AssertionConsumerServiceIndex")
		virtualIDPID = IDHash(virtualIDPID)
	}

	for _, nameIDFormat = range nameIDFormats {
		if found := spMd.Query1(nil, "./md:SPSSODescriptor/md:NameIDFormat[.="+strconv.Quote(nameIDFormat)+"]") != ""; found {
			break
		}
	}

	request.QueryDashP(nil, "./samlp:NameIDPolicy/@Format", nameIDFormat, nil)

	if wantRequesterID {
		request.QueryDashP(nil, "./samlp:Scoping/samlp:RequesterID", request.Query1(nil, "./saml:Issuer"), nil)
		if virtualIDPID != idpMd.Query1(nil, "@entityID") { // add virtual idp to wayf extension if mapped
			request.QueryDashP(nil, "./samlp:Scoping/samlp:RequesterID[0]", virtualIDPID, nil)
		}
	}

	sRequest := SamlRequest{
		Nonce:                  msgID,
		RequestID:              ID,
		SP:                     IDHash(issuer),
		VirtualIDPID:           virtualIDPID,
		NameIDFormat:           NameIDMap[nameIDFormat],
		AssertionConsumerIndex: acsIndex,
		SPIndex:                spIndex,
		HubBirkIndex:           hubBirkIndex,
		Protocol:               protocol,
	}

	buf, n := sRequest.Marshal()

	// session.Set(w, r, prefix+idHash(id), domain, bytes, authnRequestCookie, authnRequestTTL)
	// Experimental use of @ID for saving info on the original request - we will get it back as @inResponseTo
	encodedSRequest, err := AuthnRequestCookie.SpcEncode("id", buf, n)
	if err != nil {
		return
	}

	request.QueryDashP(nil, "./@ID", "_"+encodedSRequest, nil)
	return
}

// NewResponse - create a new response using the supplied metadata and resp. authnrequest and response for filling out the fields
// The response is primarily for the attributes, but other fields is eg. the AuthnContextClassRef is also drawn from it
func NewResponse(idpMd, spMd, authnrequest, sourceResponse *goxml.Xp) (response *goxml.Xp) {
	template := `<samlp:Response xmlns:samlp="urn:oasis:names:tc:SAML:2.0:protocol" xmlns:saml="urn:oasis:names:tc:SAML:2.0:assertion" Version="2.0" xmlns:xs="http://www.w3.org/2001/XMLSchema">
	<saml:Issuer></saml:Issuer>
	<samlp:Status>
		<samlp:StatusCode Value="urn:oasis:names:tc:SAML:2.0:status:Success" />
	</samlp:Status>
	<saml:Assertion xmlns:saml="urn:oasis:names:tc:SAML:2.0:assertion" Version="2.0">
		<saml:Issuer></saml:Issuer>
		<saml:Subject>
			<saml:NameID></saml:NameID>
			<saml:SubjectConfirmation Method="urn:oasis:names:tc:SAML:2.0:cm:bearer">
				<saml:SubjectConfirmationData/>
			</saml:SubjectConfirmation>
		</saml:Subject>
		<saml:Conditions>
			<saml:AudienceRestriction>
				<saml:Audience>
				</saml:Audience>
			</saml:AudienceRestriction>
		</saml:Conditions>
		<saml:AuthnStatement>
			<saml:AuthnContext>
				<saml:AuthnContextClassRef>
				</saml:AuthnContextClassRef>
			</saml:AuthnContext>
		</saml:AuthnStatement>
	</saml:Assertion>
</samlp:Response>
`
	response = goxml.NewXpFromString(template)

	issueInstant, msgID, assertionID, assertionNotOnOrAfter, sessionNotOnOrAfter := IDAndTiming()
	assertionIssueInstant := issueInstant

	spEntityID := spMd.Query1(nil, `/md:EntityDescriptor/@entityID`)
	idpEntityID := idpMd.Query1(nil, `/md:EntityDescriptor/@entityID`)

	acs := authnrequest.Query1(nil, "@AssertionConsumerServiceURL")
	response.QueryDashP(nil, "./@ID", msgID, nil)
	response.QueryDashP(nil, "./@IssueInstant", issueInstant, nil)
	response.QueryDashP(nil, "./@InResponseTo", authnrequest.Query1(nil, "@ID"), nil)
	response.QueryDashP(nil, "./@Destination", acs, nil)
	response.QueryDashP(nil, "./saml:Issuer", idpEntityID, nil)

	assertion := response.Query(nil, "saml:Assertion")[0]
	response.QueryDashP(assertion, "@ID", assertionID, nil)
	response.QueryDashP(assertion, "@IssueInstant", assertionIssueInstant, nil)
	response.QueryDashP(assertion, "saml:Issuer", idpEntityID, nil)

	nameid := response.Query(assertion, "saml:Subject/saml:NameID")[0]
	response.QueryDashP(nameid, "@SPNameQualifier", spEntityID, nil)
	response.QueryDashP(nameid, "@Format", Transient, nil)
	response.QueryDashP(nameid, ".", ID(), nil)

	subjectconfirmationdata := response.Query(assertion, "saml:Subject/saml:SubjectConfirmation/saml:SubjectConfirmationData")[0]
	response.QueryDashP(subjectconfirmationdata, "@NotOnOrAfter", assertionNotOnOrAfter, nil)
	response.QueryDashP(subjectconfirmationdata, "@Recipient", acs, nil)
	response.QueryDashP(subjectconfirmationdata, "@InResponseTo", authnrequest.Query1(nil, "@ID"), nil)

	conditions := response.Query(assertion, "saml:Conditions")[0]
	response.QueryDashP(conditions, "@NotBefore", assertionIssueInstant, nil)
	response.QueryDashP(conditions, "@NotOnOrAfter", assertionNotOnOrAfter, nil)
	response.QueryDashP(conditions, "saml:AudienceRestriction/saml:Audience", spEntityID, nil)

	authstatement := response.Query(assertion, "saml:AuthnStatement")[0]
	response.QueryDashP(authstatement, "@AuthnInstant", assertionIssueInstant, nil)
	response.QueryDashP(authstatement, "@SessionIndex", ID(), nil)
	response.QueryDashP(authstatement, "@SessionNotOnOrAfter", sessionNotOnOrAfter, nil)
	//response.QueryDashP(authstatement, "@SessionIndex", "missing", nil)

	if sourceResponse != nil {
		ac := sourceResponse.Query1(nil, `//saml:AttributeStatement/saml:Attribute[@Name="AuthnContextClassRef"]/saml:AttributeValue`)
		if ac == "" {
			ac = "urn:oasis:names:tc:SAML:2.0:ac:classes:unspecified"
		}
		response.QueryDashP(authstatement, "saml:AuthnContext/saml:AuthnContextClassRef", ac, nil)
		for _, aa := range sourceResponse.QueryMulti(nil, "//saml:AuthnContext/saml:AuthenticatingAuthority") {
			response.QueryDashP(authstatement, "saml:AuthnContext/saml:AuthenticatingAuthority[0]", aa, nil)
		}
		response.QueryDashP(nameid, "@Format", sourceResponse.Query1(nil, "//saml:NameID/@Format"), nil)
		response.QueryDashP(nameid, ".", sourceResponse.Query1(nil, "//saml:NameID"), nil)
		response.QueryDashP(authstatement, "saml:AuthnContext/saml:AuthenticatingAuthority[0]", sourceResponse.Query1(nil, "./saml:Issuer"), nil)
		response.QueryDashP(authstatement, "saml:AuthnContext/saml:AuthnContextClassRef", sourceResponse.Query1(nil, "//saml:AuthnContextClassRef"), nil)
	}
	return
}

// request2samlRequest does the protocol translation from ws-fed to saml
func request2samlRequest(r *http.Request, issuerMdSets, destinationMdSets MdSets) (msg, relayState string, err error) {
	if r.Form.Get("wa") == "wsignin1.0" || r.Form.Get("response_type") != "" {
		relayState = r.Form.Get("wctx") + r.Form.Get("state")
		issuer := r.Form.Get("wtrealm") + r.Form.Get("client_id")
		acs := r.Form.Get("wreply") + r.Form.Get("redirect_uri")

		samlrequest := goxml.NewXpFromString(`<samlp:AuthnRequest xmlns:samlp="urn:oasis:names:tc:SAML:2.0:protocol" Version="2.0"
        	                                          ProtocolBinding="urn:oasis:names:tc:SAML:2.0:bindings:HTTP-POST"/>`)
		issueInstant, msgID, _, _, _ := IDAndTiming()
		samlrequest.QueryDashP(nil, "./@ID", msgID, nil)
		samlrequest.QueryDashP(nil, "./@IssueInstant", issueInstant, nil)
		samlrequest.QueryDashP(nil, "./@Destination", "https://"+r.Host+r.URL.Path, nil)
		samlrequest.QueryDashP(nil, "./@AssertionConsumerServiceURL", acs, nil)
		samlrequest.QueryDashP(nil, "./saml:Issuer", issuer, nil)
		protocol := samlrequest.QueryDashP(nil, "./samlp:Extensions/wayf:protocol", "", nil)
		if r.Form.Get("wa") == "wsignin1.0" {
			samlrequest.QueryDashP(protocol, ".", "wsfed", nil)
		} else if r.Form.Get("response_type") != "" {
			samlrequest.QueryDashP(protocol, ".", "oauth", nil)
			samlrequest.QueryDashP(nil, "./@ID", r.Form.Get("nonce"), nil)
			relayState = r.Form.Get("state")
		}

		DumpFileIfTracing(r, samlrequest)
		msg = base64.StdEncoding.EncodeToString(Deflate(samlrequest.Dump()))
		return
	}
	err = fmt.Errorf("no SAMLRequest/SAMLResponse found")
	return

}

// NewWsFedResponse generates a Ws-fed response
func NewWsFedResponse(idpMd, spMd, sourceResponse *goxml.Xp) (response *goxml.Xp) {
	template := `<t:RequestSecurityTokenResponse xmlns:t="http://schemas.xmlsoap.org/ws/2005/02/trust" xmlns:wsu="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-utility-1.0.xsd"
    xmlns:wsp="http://schemas.xmlsoap.org/ws/2004/09/policy" xmlns:wsa="http://www.w3.org/2005/08/addressing" xmlns:saml1="urn:oasis:names:tc:SAML:1.0:assertion">
	<t:Lifetime>
		<wsu:Created></wsu:Created>
		<wsu:Expires></wsu:Expires>
	</t:Lifetime>
	<wsp:AppliesTo><wsa:EndpointReference><wsa:Address></wsa:Address></wsa:EndpointReference></wsp:AppliesTo>
	<t:RequestedSecurityToken>
		<saml1:Assertion MajorVersion="1" MinorVersion="1">
			<saml1:Conditions>
				<saml1:AudienceRestrictionCondition><saml1:Audience></saml1:Audience></saml1:AudienceRestrictionCondition>
			</saml1:Conditions>
			<saml1:AttributeStatement>
			    <saml1:Subject></saml1:Subject>
			</saml1:AttributeStatement>
			<saml1:AuthenticationStatement>
			    <saml1:Subject></saml1:Subject>
			</saml1:AuthenticationStatement>
		</saml1:Assertion>
	</t:RequestedSecurityToken>
	<t:TokenType>urn:oasis:names:tc:SAML:1.0:assertion</t:TokenType>
	<t:RequestType>http://schemas.xmlsoap.org/ws/2005/02/trust/Issue</t:RequestType>
	<t:KeyType>http://schemas.xmlsoap.org/ws/2005/05/identity/NoProofKey</t:KeyType>
</t:RequestSecurityTokenResponse>
`
	response = goxml.NewXpFromString(template)

	issueInstant, _, assertionID, _, sessionNotOnOrAfter := IDAndTiming()
	assertionIssueInstant := issueInstant

	spEntityID := spMd.Query1(nil, `/md:EntityDescriptor/@entityID`)
	idpEntityID := idpMd.Query1(nil, `/md:EntityDescriptor/@entityID`)

	response.QueryDashP(nil, "./t:Lifetime/wsu:Created", issueInstant, nil)
	response.QueryDashP(nil, "./t:Lifetime/wsu:Expires", sessionNotOnOrAfter, nil)
	response.QueryDashP(nil, "./wsp:AppliesTo/wsa:EndpointReference/wsa:Address", spEntityID, nil)

	assertion := response.Query(nil, "t:RequestedSecurityToken/saml1:Assertion")[0]
	response.QueryDashP(assertion, "@AssertionID", assertionID, nil)
	response.QueryDashP(assertion, "@IssueInstant", assertionIssueInstant, nil)
	response.QueryDashP(assertion, "@Issuer", idpEntityID, nil)

	conditions := response.Query(assertion, "saml1:Conditions")[0]
	response.QueryDashP(conditions, "@NotBefore", assertionIssueInstant, nil)
	response.QueryDashP(conditions, "@NotOnOrAfter", sessionNotOnOrAfter, nil)
	response.QueryDashP(conditions, "saml1:AudienceRestrictionCondition/saml1:Audience", spEntityID, nil)

	nameIdentifierElement := sourceResponse.Query(nil, "./saml:Assertion/saml:Subject/saml:NameID")[0]
	nameIdentifier := sourceResponse.Query1(nameIdentifierElement, ".")
	nameIDFormat := sourceResponse.Query1(nameIdentifierElement, "./@Format")

	authStmt := response.Query(assertion, "saml1:AuthenticationStatement")[0]
	response.QueryDashP(authStmt, "@AuthenticationInstant", assertionIssueInstant, nil)

	for _, stmt := range response.Query(assertion, ".//saml1:Subject") {
		response.QueryDashP(stmt, "saml1:NameIdentifier", nameIdentifier, nil)
		response.QueryDashP(stmt, "saml1:NameIdentifier/@Format", nameIDFormat, nil)
		response.QueryDashP(stmt, "saml1:SubjectConfirmation/saml1:ConfirmationMethod", "urn:oasis:names:tc:SAML:1.0:cm:bearer", nil)
	}

	authContext := sourceResponse.Query1(nil, "./saml:Assertion/saml:AuthnStatement/saml:AuthnContext/saml:AuthnContextClassRef")
	response.QueryDashP(authStmt, "./@AuthenticationMethod", authContext, nil)

	return
}

// SamlTime2JwtTime - convert string SAML time to epoch
func SamlTime2JwtTime(xmlTime string) int64 {
	samlTime, _ := time.Parse(XsDateTime, xmlTime)
	return samlTime.Unix()
}

// Jwt2saml - JSON based IdP interface
func Jwt2saml(w http.ResponseWriter, r *http.Request, mdHub, mdInternal, mdExternalIDP, mdExternalSP Md, requestHandler func(*goxml.Xp, *goxml.Xp, *goxml.Xp) (map[string][]string, error), signerMd *goxml.Xp) (err error) {
	defer r.Body.Close()
	r.ParseForm()

	request, spMd, idpMd, _, _, _, err := ReceiveAuthnRequest(r, MdSets{mdHub, mdExternalSP}, MdSets{mdInternal, mdExternalIDP}, r.Form.Get("sso"))
	if err != nil {
		return
	}

	jwt := r.Form.Get("jwt")
	if jwt == "" {
		req, err := requestHandler(request, idpMd, spMd)
		if err != nil {
			return err
		}
		json, err := json.MarshalIndent(&req, "  ", "  ")
		if err != nil {
			return err
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", strconv.Itoa(len(json)))
		w.Write(json)
		return err
	}
	zzz, err := jwtVerify(jwt, idpMd.QueryMulti(nil, "./md:IDPSSODescriptor"+SigningCertQuery))
	if err != nil {
		return err
	}
	payload, _ := base64.RawURLEncoding.DecodeString(zzz)
	var attrs map[string]interface{}
	err = json.Unmarshal(payload, &attrs)
	if err != nil {
		return err
	}

	response := NewResponse(idpMd, spMd, request, nil)

	if iat, ok := attrs["iat"]; ok {
		delete(attrs, "iat")
		if math.Abs(float64(time.Now().Unix())-iat.(float64)) > timeskew {
			return fmt.Errorf("jwt timed out")
		}
	}
	if aas := attrs["saml:AuthenticatingAuthority"]; aas != nil {
		for _, aa := range aas.([]interface{}) {
			response.QueryDashP(nil, "./saml:Assertion/saml:AuthnStatement/saml:AuthnContext/saml:AuthenticatingAuthority[0]", aa.(string), nil)
		}
		delete(attrs, "saml:AuthenticatingAuthority")
	}

	destinationAttributes := response.QueryDashP(nil, `/saml:Assertion/saml:AttributeStatement[1]`, "", nil)
	for name, vals := range attrs {
		attr := response.QueryDashP(destinationAttributes, `saml:Attribute[@Name=`+strconv.Quote(name)+`]`, "", nil)
		response.QueryDashP(attr, `@NameFormat`, "urn:oasis:names:tc:SAML:2.0:attrname-format:basic", nil)
		switch v := vals.(type) {
		case []interface{}:
			for _, value := range v {
				response.QueryDashP(attr, "saml:AttributeValue[0]", value.(string), nil)
			}
		}
	}

	err = SignResponse(response, "/samlp:Response/saml:Assertion", signerMd, "sha256", SAMLSign)
	if err != nil {
		return err
	}
	if spMd.QueryXMLBool(nil, "/md:EntityDescriptor/md:Extensions/wayf:wayf/wayf:assertion.encryption") {
		cert := spMd.Query1(nil, "./md:SPSSODescriptor"+EncryptionCertQuery) // actual encryption key is always first
		_, publicKey, _ := PublicKeyInfo(cert)
		ea := goxml.NewXpFromString(`<saml:EncryptedAssertion xmlns:saml="urn:oasis:names:tc:SAML:2.0:assertion"></saml:EncryptedAssertion>`)
		assertion := response.Query(nil, "saml:Assertion[1]")[0]
		err = response.Encrypt(assertion, publicKey, ea)
		fmt.Println("err", err)
	}

	data := Formdata{Acs: response.Query1(nil, "./@Destination"), Samlresponse: base64.StdEncoding.EncodeToString(response.Dump()), RelayState: r.Form.Get("RelayState")}
	return PostForm.ExecuteTemplate(w, "postForm", data)

}

// Saml2jwt - JSON based SP interface
func Saml2jwt(w http.ResponseWriter, r *http.Request, mdHub, mdInternal, mdExternalIDP, mdExternalSP Md, requestHandler func(*goxml.Xp, *goxml.Xp, *goxml.Xp) (map[string][]string, error), defaultIdpentityid string, allowedDigestAndSignatureAlgorithms []string, signingMethodPath string) (err error) {
	defer r.Body.Close()
	r.ParseForm()

	// backward compatible - use either or
	entityID := r.Header.Get("X-Issuer") + r.Form.Get("issuer")

	spMd, _, err := FindInMetadataSets(MdSets{mdInternal, mdExternalSP}, entityID)
	if err != nil {
		return
	}

	idpentityid := r.Form.Get("idpentityid")
	if idpentityid == "" {
		idpentityid = defaultIdpentityid
	}

	app := r.Header.Get("X-App") + r.Form.Get("app")
	acs := r.Header.Get("X-Acs") + r.Form.Get("acs")

	if _, ok := r.Form["SAMLResponse"]; ok {
		response, idpMd, _, relayState, _, _, err := DecodeSAMLMsg(r, MdSets{mdHub, mdExternalIDP}, MdSets{mdInternal, mdExternalSP}, SPRole, []string{"Response", "LogoutResponse"}, acs, nil)
		if err != nil {
			return err
		}
		switch response.QueryString(nil, "local-name(/*)") {
		case "Response":

			if err = CheckDigestAndSignatureAlgorithms(response, allowedDigestAndSignatureAlgorithms, idpMd.QueryMulti(nil, signingMethodPath)); err != nil {
				return err
			}
			if _, err = requestHandler(response, idpMd, spMd); err != nil {
				return err
			}
			attrs := map[string]interface{}{}

			assertion := response.Query(nil, "/samlp:Response/saml:Assertion")[0]
			names := response.QueryMulti(assertion, "saml:AttributeStatement/saml:Attribute/@Name")
			for _, name := range names {
				attrs[name] = response.QueryMulti(assertion, "saml:AttributeStatement/saml:Attribute[@Name="+strconv.Quote(name)+"]/saml:AttributeValue")
			}

			attrs["iss"] = response.Query1(assertion, "./saml:Issuer")
			attrs["aud"] = response.Query1(assertion, "./saml:Conditions/saml:AudienceRestriction/saml:Audience")
			attrs["nbf"] = SamlTime2JwtTime(response.Query1(assertion, "./saml:Conditions/@NotBefore"))
			attrs["exp"] = SamlTime2JwtTime(response.Query1(assertion, "./saml:Conditions/@NotOnOrAfter"))
			attrs["iat"] = SamlTime2JwtTime(response.Query1(assertion, "@IssueInstant"))
			attrs["saml:AuthenticatingAuthority"] = response.QueryMulti(assertion, "./saml:AuthnStatement/saml:AuthnContext/saml:AuthenticatingAuthority")
			//attrs["saml:AuthenticatingAuthority"] = append(attrs["saml:AuthenticatingAuthority"].([]string), attrs["iss"].(string))

			//sloinfoJson, _ := json.Marshal(NewSLOInfo(response, spMd.Query1(nil, "@entityID")))
			//attrs["sloinfo"] = sloinfoJson

			json, err := json.Marshal(&attrs)
			if err != nil {
				return err
			}
			privatekey, _, err := GetPrivateKey(idpMd)
			if err != nil {
				return err
			}
			jwt, _, err := JwtSign(json, privatekey)
			if err != nil {
				return err
			}

			w.Header().Set("Authorization", "Bearer "+jwt)

			app, err := AuthnRequestCookie.Decode("app", relayState)
			if err != nil {
				return err
			}

			w.Header().Set("X-Accel-Redirect", string(app))
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(jwt))
			return err
		case "LogoutResponse":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`abc.` + base64.URLEncoding.EncodeToString([]byte(`{"abc":"Logout OK"}`)) + `.def`))
			return nil
		}
	} else if _, ok := r.Form["SAMLRequest"]; ok {
		request, idpMd, _, _, _, _, err := DecodeSAMLMsg(r, MdSets{mdHub, mdExternalIDP}, MdSets{mdInternal, mdExternalSP}, SPRole, []string{"LogoutRequest"}, acs, nil)
		if err != nil {
			return err
		}
		SloResponse(w, r, request, spMd, idpMd, "")
		return nil
	} else if sloinfoJSON := r.Form.Get("sloinfo"); sloinfoJSON != "" {
		relayState := ""
		sloinfoTxt, _ := base64.StdEncoding.DecodeString(sloinfoJSON)
		sloinfo := &SLOInfo{}
		err = json.Unmarshal([]byte(sloinfoTxt), &sloinfo)
		if err != nil {
			return err
		}
		sloinfo.IssuerID, sloinfo.DestinationID = sloinfo.DestinationID, sloinfo.IssuerID
		idpMd, _, err := FindInMetadataSets(MdSets{mdHub, mdExternalIDP}, sloinfo.DestinationID)
		if err != nil {
			return err
		}
		request, _, err := NewLogoutRequest(idpMd, sloinfo, IDPRole)
		if err != nil {
			return err
		}
		u, err := SAMLRequest2URL(request, relayState, "", "", "")
		if err != nil {
			return err
		}

		http.Redirect(w, r, u.String(), http.StatusFound)
		return err
	} else if idpentityid != "" {
		idpMd, _, err := FindInMetadataSets(MdSets{mdHub, mdExternalIDP}, idpentityid)
		if err != nil {
			return err
		}

		relayState, err := AuthnRequestCookie.Encode("app", []byte(app))
		if err != nil {
			return err
		}

		request, err := NewAuthnRequest(nil, spMd, idpMd, "", strings.Split(r.Form.Get("idplist"), ","), acs, false, 0, 0)
		if err != nil {
			return err
		}

		u, err := SAMLRequest2URL(request, relayState, "", "", "")
		if err != nil {
			return err
		}

		http.Redirect(w, r, u.String(), http.StatusFound)
		return err
	} else {
		discoveryURLTemplate := `https://wayf.wayf.dk/ds/?returnIDParam=idpentityid&entityID={{.EntityID}}&return={{.ACS}}`
		discoveryURL := template.Must(template.New("discoveryURL").Parse(discoveryURLTemplate))
		buf := new(bytes.Buffer)
		discoveryURL.Execute(buf, struct{ EntityID, ACS string }{entityID, acs})
		http.Redirect(w, r, buf.String(), http.StatusFound)
		return
	}
	return
}

// JwtSign - sign a json payload, return jwt and at_atHash
func JwtSign(json []byte, privatekey []byte) (jwt, atHash string, err error) {
	payload := base64.RawURLEncoding.EncodeToString(json)
	//header := "eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9." // sha256
	//dgst := sha256.Sum256
	header := "eyJhbGciOiJSUzUxMiIsInR5cCI6IkpXVCJ9." // sha512
	dgst := sha512.Sum512

	digest := dgst([]byte(header + payload))
	signature, err := goxml.Sign(digest[:], privatekey, []byte("-"), "sha512") // "sha256"
	if err != nil {
		err = goxml.Wrap(err)
		return
	}
	jwt = header + payload + "." + base64.RawURLEncoding.EncodeToString(signature)
	atHashDigest := dgst([]byte(jwt))
	atHash = base64.RawURLEncoding.EncodeToString(atHashDigest[:len(atHashDigest)/2])
	return
}

func jwtVerify(jwt string, certificates []string) (payload string, err error) {
	if len(certificates) == 0 {
		return payload, errors.New("No Certs found")
	}
	hps := strings.SplitN(jwt, ".", 3)
	hp := []byte(strings.Join(hps[:2], "."))
	headerJSON, _ := base64.RawURLEncoding.DecodeString(hps[0])
	header := struct{ Alg string }{}
	err = json.Unmarshal(headerJSON, &header)
	if err != nil {
		return
	}
	var hh crypto.Hash
	var digest []byte
	switch header.Alg {
	case "RS256":
		dg := sha256.Sum256(hp)
		digest = dg[:]
		hh = crypto.SHA256
	case "RS512":
		dg := sha512.Sum512(hp)
		digest = dg[:]
		hh = crypto.SHA512
	default:
		return payload, fmt.Errorf("Unsupported alg: %s", header.Alg)
	}

	sign, _ := base64.RawURLEncoding.DecodeString(hps[2])
	var pub *rsa.PublicKey
	for _, certificate := range certificates {
		_, pub, err = PublicKeyInfo(certificate)
		if err != nil {
			return
		}
		err = rsa.VerifyPKCS1v15(pub, hh, digest, sign)
		if err == nil {
			return hps[1], err
		}
	}
	return
}

// CheckDigestAndSignatureAlgorithms -
func CheckDigestAndSignatureAlgorithms(response *goxml.Xp, allowedDigestAndSignatureAlgorithms, xtraAlgos []string) (err error) {
	contexts := []string{"/samlp:Response/ds:Signature/ds:SignedInfo/", "/samlp:Response/saml:Assertion/ds:Signature/ds:SignedInfo/"}
	paths := []string{"ds:SignatureMethod/@Algorithm", "ds:Reference/ds:DigestMethod/@Algorithm"}
	seen := 0
	allowedAlgosMap := map[string]bool{}
	for _, algo := range allowedDigestAndSignatureAlgorithms {
		allowedAlgosMap[algo] = true
	}
	for _, algo := range xtraAlgos {
		allowedAlgosMap[goxml.Algos[algo].Short] = true
	}
	for _, context := range contexts {
		for _, path := range paths {
			algo := response.Query1(nil, context+path)
			if algo != "" {
				if !allowedAlgosMap[goxml.Algos[algo].Short] {
					return fmt.Errorf("Unsupported Digest/Signing algorithm: %s", algo)
				}
				seen++
			}
		}
	}
	if seen < 2 {
		return fmt.Errorf("No or to few Digest/Signing algoritms found")
	}
	return
}

// Marshal - hand-held marshal for SLOInfo struct
func (r SLOInfo) Marshal() (msg []byte) {
	for _, str := range []string{r.IssuerID, r.NameID, r.SPNameQualifier, r.SessionIndex, r.DestinationID} {
		msg = append(msg, 0xd9, uint8(len(str)))
		msg = append(msg, str...)
	}
	msg = append(msg, 0xcc, r.NameIDFormat)
	return
}

// Unmarshal - hand-held unmarshal for SLOInfo struct
func (r *SLOInfo) Unmarshal(msg []byte) {
	i := byte(2)
	l := i + msg[i-1]
	for _, x := range []*string{&r.IssuerID, &r.NameID, &r.SPNameQualifier, &r.SessionIndex} {
		*x = string(msg[i:l])
		i = l + 2
		l = i + msg[i-1]
	}
	r.DestinationID = string(msg[i:l])
	i = l + 1
	r.NameIDFormat = msg[i]
	return
}

// Encode using hand-held MessagePack for keeping the size down - no double base64 encodings
func (h *Hm) Encode(id string, msg []byte) (str string, err error) {
	bts, err := h.innerSign(id, msg, time.Now().Unix())
	return base64.RawURLEncoding.EncodeToString(bts), err
}

// SpcEncode - does not base64 encodes the msg from num bytes onward - ie. it is already in
// an allowed format for whatever purpose it is intended - this is to save space by not base64
// encode data that does not need it
func (h *Hm) SpcEncode(id string, msg []byte, num int) (str string, err error) {
	bts, err := h.innerSign(id, msg, time.Now().Unix())
	str = base64.RawURLEncoding.EncodeToString(bts[:24+num]) + string(msg[num:]) // 24 is the size of hmac + timestamp in MP format
	return
}

// Decode - the whole message
func (h *Hm) Decode(id, in string) ([]byte, error) {
	signedMsg, _ := base64.RawURLEncoding.DecodeString(in)
	return h.innerValidate(id, signedMsg)
}

// SpcDecode - only base64 decode specified number of bytes
func (h *Hm) SpcDecode(id, in string, num int) ([]byte, error) {
	encoded, _ := base64.RawURLEncoding.DecodeString(in[:40]) // len(base64 encoded(24+num)) == 40
	encoded = append(encoded, in[40:]...)
	return h.innerValidate(id, encoded)
}

func (h *Hm) innerSign(id string, msg []byte, ts int64) (signedMsg []byte, err error) {
	bs := make([]byte, 4)
	binary.BigEndian.PutUint32(bs, uint32(ts))
	hash := hmac.New(h.Hash, h.Key)
	hash.Write([]byte(id))
	hash.Write([]byte(bs))
	hash.Write(msg)

	signedMsg = append(signedMsg, 0xc4, 0x10)
	signedMsg = append(signedMsg, hash.Sum(nil)[:16]...)
	signedMsg = append(signedMsg, 0xd6, 0xFF)
	signedMsg = append(signedMsg, bs...)
	signedMsg = append(signedMsg, msg...)
	return signedMsg, nil
}

func (h *Hm) innerValidate(id string, signedMsg []byte) (msg []byte, err error) {
	ts := int64(binary.BigEndian.Uint32(signedMsg[20:24]))
	msg = signedMsg[24:]
	computed, err := h.innerSign(id, msg, ts)
	if err != nil {
		return
	}
	if hmac.Equal(signedMsg[:24], []byte(computed)[:24]) {
		now := time.Now().Unix()
		if now-ts < h.TTL {
			return msg, nil
		}
	}
	return nil, goxml.NewWerror("hmac failed")
}

// PP - super simple Pretty Print - using JSON
func PP(i ...interface{}) {
	for _, e := range i {
		s, _ := json.MarshalIndent(e, "", "\t")
		fmt.Println(string(s))
	}
	return
}

// Marshal hand-held marshal SamlRequest
func (r SamlRequest) Marshal() (msg []byte, n int) {
	prefix := []byte{}
	for _, str := range []string{r.Nonce, r.RequestID, r.SP, r.VirtualIDPID, r.AssertionConsumerIndex, r.Protocol} {
		prefix = append(prefix, uint8(len(str))) // if over 127 we are in trouble
		msg = append(msg, str...)
	}
	str := fmt.Sprintf("%1d%1d%1d", r.NameIDFormat, r.SPIndex, r.HubBirkIndex)
	msg = append(msg, str...)
	msg = append(prefix, msg...)
	n = len(prefix)
	PP("marshal", r)
	return
}

// Unmarshal - hand held unmarshal for SamlRequest
func (r *SamlRequest) Unmarshal(msg []byte) {
	i := byte(6)
	l := msg[0]
	for j, x := range []*string{&r.Nonce, &r.RequestID, &r.SP, &r.VirtualIDPID, &r.AssertionConsumerIndex, &r.Protocol} {
		*x = string(msg[i : i+l])
		i = i + l
		l = msg[j+1]
	}
	r.NameIDFormat = msg[i] - 48 // cheap char to int8
	r.SPIndex = msg[i+1] - 48
	r.HubBirkIndex = msg[i+2] - 48
	PP("unmarshal", r)
	return
}
