package kubeconfig

// ClusterInfo holds the cluster data
type ClusterInfo struct {
	CertificateAuthorityData string `json:"certificate-authority-data"`
	ProxyURL                 string `json:"proxy-url,omitempty"`
	Server                   string `json:"server"`
}

// Clusters hold an array of the clusters that would exist in the config file
type Clusters []struct {
	Cluster ClusterInfo `json:"cluster"`
	Name    string      `json:"name"`
}

// Context holds the cluster context
type Context struct {
	Cluster string `json:"cluster"`
	User    string `json:"user"`
}

// Contexts holds an array of the contexts
type Contexts []struct {
	Context Context `json:"context"`
	Name    string  `json:"name"`
}

// Users holds an array of the users that would exist in the config file
type Users []struct {
	CertInfo CertInfo `json:"user"`
	Name     string   `json:"name"`
}

// CertInfo holds the user's credentials: a client certificate and key, a bearer token, or both,
// as a kubeconfig user carries them (clientcmd's AuthInfo: client-certificate-data, client-key-data,
// token).
type CertInfo struct {
	ClientCertificateData string `json:"client-certificate-data"`
	ClientKeyData         string `json:"client-key-data"`
	Token                 string `json:"token,omitempty"`
}

// KubeConfig holds the necessary data for creating a new KubeConfig file
type KubeConfig struct {
	APIVersion     string   `json:"apiVersion"`
	Clusters       Clusters `json:"clusters"`
	Contexts       Contexts `json:"contexts"`
	CurrentContext string   `json:"current-context"`
	Kind           string   `json:"kind"`
	Users          Users    `json:"users"`
}
