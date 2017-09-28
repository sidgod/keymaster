package twofa

import (
	"bufio"
	"bytes"
	"crypto"
	"crypto/tls"
	"encoding/json"
	"encoding/pem"
	"github.com/Symantec/Dominator/lib/log"
	// client side (interface with hardware)
	"github.com/flynn/u2f/u2fhid"
	// server side:
	"crypto/x509"
	"errors"
	"fmt"
	"github.com/Symantec/keymaster/lib/client/twofa/u2f"
	"github.com/Symantec/keymaster/lib/client/util"
	"github.com/Symantec/keymaster/lib/webapi/v0/proto"
	"golang.org/x/crypto/ssh"
	"io"
	"io/ioutil"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
)

const clientDataAuthenticationTypeValue = "navigator.id.getAssertion"

// This is now copy-paste from the server test side... probably make public and reuse.
func createKeyBodyRequest(method, urlStr, filedata string) (*http.Request, error) {
	//create attachment....
	bodyBuf := &bytes.Buffer{}
	bodyWriter := multipart.NewWriter(bodyBuf)

	//
	fileWriter, err := bodyWriter.CreateFormFile("pubkeyfile", "somefilename.pub")
	if err != nil {
		fmt.Println("error writing to buffer")
		return nil, err
	}
	// When using a file this used to be: fh, err := os.Open(pubKeyFilename)
	fh := strings.NewReader(filedata)

	_, err = io.Copy(fileWriter, fh)
	if err != nil {
		return nil, err
	}

	err = bodyWriter.WriteField("duration", (*Duration).String())
	if err != nil {
		return nil, err
	}

	contentType := bodyWriter.FormDataContentType()
	bodyWriter.Close()

	req, err := http.NewRequest(method, urlStr, bodyBuf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", contentType)

	return req, nil
}

func doCertRequest(client *http.Client, authCookies []*http.Cookie, url, filedata string, logger log.Logger) ([]byte, error) {

	req, err := createKeyBodyRequest("POST", url, filedata)
	if err != nil {
		return nil, err
	}
	// Add the login cookies
	for _, cookie := range authCookies {
		req.AddCookie(cookie)
	}
	resp, err := client.Do(req) // Client.Get(targetUrl)
	if err != nil {
		logger.Printf("Failure to do x509 req %s", err)
		return nil, err
	}

	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		logger.Printf("got error from call %s, url='%s'\n", resp.Status, url)
		return nil, err
	}
	return ioutil.ReadAll(resp.Body)

}

func doVIPAuthenticate(
	client *http.Client,
	authCookies []*http.Cookie,
	baseURL string,
	logger log.DebugLogger) error {
	logger.Printf("top of doVIPAuthenticate")

	// Read VIP token from client

	reader := bufio.NewReader(os.Stdin)
	fmt.Print("Enter VIP/OTP code: ")
	otpText, err := reader.ReadString('\n')
	otpText = strings.TrimSpace(otpText)
	//fmt.Println(codeText)
	logger.Debugf(1, "codeText:  '%s'", otpText)

	// TODO: add some client side validation that the codeText is actually a six digit
	// integer

	VIPLoginURL := baseURL + "/api/v0/vipAuth"

	form := url.Values{}
	form.Add("OTP", otpText)
	//form.Add("password", string(password[:]))
	req, err := http.NewRequest("POST", VIPLoginURL, strings.NewReader(form.Encode()))

	// Add the login cookies
	for _, cookie := range authCookies {
		req.AddCookie(cookie)
	}
	logger.Debugf(0, "Authcookies:  %+v", authCookies)

	req.Header.Add("Content-Length", strconv.Itoa(len(form.Encode())))
	req.Header.Add("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Add("Accept", "application/json")

	loginResp, err := client.Do(req) //client.Get(targetUrl)
	if err != nil {
		logger.Printf("got error from req")
		logger.Println(err)
		// TODO: differentiate between 400 and 500 errors
		// is OK to fail.. try next
		return err
	}
	defer loginResp.Body.Close()
	if loginResp.StatusCode != 200 {
		logger.Printf("got error from login call %s", loginResp.Status)
		return err
	}

	loginJSONResponse := proto.LoginResponse{}
	//body := jsonrr.Result().Body
	err = json.NewDecoder(loginResp.Body).Decode(&loginJSONResponse)
	if err != nil {
		return err
	}
	io.Copy(ioutil.Discard, loginResp.Body)

	logger.Debugf(1, "This the login response=%v\n", loginJSONResponse)

	return nil
}

func getCertsFromServer(
	signer crypto.Signer,
	userName string,
	password []byte,
	baseUrl string,
	tlsConfig *tls.Config,
	skip2fa bool,
	logger log.DebugLogger) (sshCert []byte, x509Cert []byte, err error) {
	//First Do Login
	client, err := util.GetHttpClient(tlsConfig)
	if err != nil {
		return nil, nil, err
	}

	loginUrl := baseUrl + proto.LoginPath
	form := url.Values{}
	form.Add("username", userName)
	form.Add("password", string(password[:]))
	req, err := http.NewRequest("POST", loginUrl, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Add("Content-Length", strconv.Itoa(len(form.Encode())))
	req.Header.Add("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Add("Accept", "application/json")

	loginResp, err := client.Do(req) //client.Get(targetUrl)
	if err != nil {
		logger.Printf("got error from req")
		logger.Println(err)
		// TODO: differentiate between 400 and 500 errors
		// is OK to fail.. try next
		return nil, nil, err
	}
	defer loginResp.Body.Close()
	if loginResp.StatusCode != 200 {
		logger.Printf("got error from login call %s", loginResp.Status)
		return nil, nil, err
	}
	//Enusre we have at least one cookie
	if len(loginResp.Cookies()) < 1 {
		err = errors.New("No cookies from login")
		return nil, nil, err
	}

	loginJSONResponse := proto.LoginResponse{}
	//body := jsonrr.Result().Body
	err = json.NewDecoder(loginResp.Body).Decode(&loginJSONResponse)
	if err != nil {
		return nil, nil, err
	}
	loginResp.Body.Close() //so that we can reuse the channel

	logger.Debugf(1, "This the login response=%v\n", loginJSONResponse)

	allowVIP := false
	allowU2F := false
	for _, backend := range loginJSONResponse.CertAuthBackend {
		if backend == proto.AuthTypePassword {
			skip2fa = true
		}
		if backend == proto.AuthTypeSymantecVIP {
			allowVIP = true
			//remote next statemente later
			//skipu2f = true
		}
		if backend == proto.AuthTypeU2F {
			allowU2F = true
		}
	}

	// Dont try U2F if chosen by user
	if *noU2F {
		allowU2F = false
	}
	if *noVIPAccess {
		allowVIP = false
	}

	// upgrade to u2f
	successful2fa := false
	if !skip2fa {
		if allowU2F {
			devices, err := u2fhid.Devices()
			if err != nil {
				logger.Fatal(err)
				return nil, nil, err
			}
			if len(devices) > 0 {

				err = u2f.DoU2FAuthenticate(
					client, loginResp.Cookies(), baseUrl, logger)
				if err != nil {

					return nil, nil, err
				}
				successful2fa = true
			}
		}

		if allowVIP && !successful2fa {
			err = doVIPAuthenticate(
				client, loginResp.Cookies(), baseUrl, logger)
			if err != nil {

				return nil, nil, err
			}
			successful2fa = true
		}

		if !successful2fa {
			err = errors.New("Failed to Pefrom 2FA (as requested from server)")
			return nil, nil, err
		}

	}

	logger.Debugf(1, "Authentication Phase complete")

	//now get x509 cert
	pubKey := signer.Public()
	derKey, err := x509.MarshalPKIXPublicKey(pubKey)
	if err != nil {
		return nil, nil, err
	}
	pemKey := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: derKey}))

	// TODO: urlencode the userName
	x509Cert, err = doCertRequest(
		client,
		loginResp.Cookies(),
		baseUrl+"/certgen/"+userName+"?type=x509",
		pemKey,
		logger)
	if err != nil {
		return nil, nil, err
	}

	//// Now we do sshCert!
	// generate and write public key
	sshPub, err := ssh.NewPublicKey(pubKey)
	if err != nil {
		return nil, nil, err
	}
	sshAuthFile := string(ssh.MarshalAuthorizedKey(sshPub))
	sshCert, err = doCertRequest(
		client,
		loginResp.Cookies(),
		baseUrl+"/certgen/"+userName+"?type=ssh",
		sshAuthFile,
		logger)
	if err != nil {
		return nil, nil, err
	}

	return sshCert, x509Cert, nil
}

func getCertFromTargetUrls(
	signer crypto.Signer,
	userName string,
	password []byte,
	targetUrls []string,
	rootCAs *x509.CertPool,
	skipu2f bool,
	logger log.DebugLogger) (sshCert []byte, x509Cert []byte, err error) {
	success := false
	tlsConfig := &tls.Config{RootCAs: rootCAs, MinVersion: tls.VersionTLS12}

	for _, baseUrl := range targetUrls {
		logger.Printf("attempting to target '%s' for '%s'\n", baseUrl, userName)
		sshCert, x509Cert, err = getCertsFromServer(
			signer, userName, password, baseUrl, tlsConfig, skipu2f, logger)
		if err != nil {
			logger.Println(err)
			continue
		}
		success = true
		break

	}
	if !success {
		err := errors.New("Failed to get creds")
		return nil, nil, err
	}

	return sshCert, x509Cert, nil
}
