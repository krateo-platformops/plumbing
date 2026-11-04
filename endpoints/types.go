package endpoints

type Endpoint struct {
	ServerURL                string `json:"serverURL"`
	ProxyURL                 string `json:"proxyURL,omitempty"`
	CertificateAuthorityData string `json:"caData,omitempty"`
	ClientCertificateData    string `json:"-"`
	ClientKeyData            string `json:"-"`
	Token                    string `json:"token,omitempty"`
	Username                 string `json:"username,omitempty"`
	Password                 string `json:"password,omitempty"`
	Debug                    bool   `json:"debug"`
	Insecure                 bool   `json:"insecure"`
	// PassCredentialsOnRedirect lets a credential follow a redirect to a DIFFERENT host:port
	// (core-provider#307). Default false, which refuses.
	//
	// NAMED FOR THE HOP, not for the destination, deliberately. helm's equivalent is
	// PassCredentialsAll, and an operator reads "all" as a statement about which destinations are
	// allowed rather than about every hop of a chain they never see. The installer session made
	// exactly that point about an allowAnyHost flag elsewhere: the people who set such a flag are
	// by definition debugging something, and they are the least likely to be thinking about
	// redirects.
	//
	// IT REMOVES THE WHOLE GUARD, not part of it. With this set, a credential will follow a 302
	// anywhere the endpoint sends it - including a subdomain, a different port, or an unrelated
	// host. There is no partial setting and that is on purpose: a half-guard would be harder to
	// reason about than none.
	//
	// It exists because the strict default has a legitimate casualty: an endpoint configured
	// without a port that redirects to one - an on-prem GHES, or a corporate proxy - is a real
	// deployment, and helm's own chart getter has carried PassCredentialsAll for that reason since
	// long before this. Measured on krateo-057: 0 of 3 external targets issue any 3xx, so nothing
	// there needs it; the field is for the deployment that does.
	PassCredentialsOnRedirect bool `json:"passCredentialsOnRedirect,omitempty"`
	// AWS garbage fields for their proprietary authentication header
	AwsAccessKey string `json:"awsAccessKey"`
	AwsSecretKey string `json:"awsSecretKey"`
	AwsRegion    string `json:"awsRegion"`
	AwsService   string `json:"awsService"`
	// AWS time field, testing only
	AwsTime string `json:"awsTime"`
}

// HasCA returns whether the configuration has a certificate authority or not.
func (ep *Endpoint) HasCA() bool {
	return len(ep.CertificateAuthorityData) > 0
}

// HasBasicAuth returns whether the configuration has basic authentication or not.
func (ep *Endpoint) HasBasicAuth() bool {
	return len(ep.Password) != 0
}

// HasTokenAuth returns whether the configuration has token authentication or not.
func (ep *Endpoint) HasTokenAuth() bool {
	return len(ep.Token) != 0
}

// HasCertAuth returns whether the configuration has certificate authentication or not.
func (ep *Endpoint) HasCertAuth() bool {
	return len(ep.ClientCertificateData) != 0 && len(ep.ClientKeyData) != 0
}

// HasAwsAuth returns whether the configuration has AWS authentication or not.
func (ep *Endpoint) HasAwsAuth() bool {
	return len(ep.AwsAccessKey) != 0 &&
		len(ep.AwsSecretKey) != 0 &&
		len(ep.AwsRegion) != 0 &&
		len(ep.AwsSecretKey) != 0
}
