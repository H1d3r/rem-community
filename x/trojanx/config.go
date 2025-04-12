package trojanx

import (
	"crypto/tls"
)

type TrojanConfig struct {
	Password           string              `json:"password"`
	TLSConfig          *TLSConfig          `json:"tls_config"`
	ReverseProxyConfig *ReverseProxyConfig `json:"reverse_proxy"`
}

type TLSConfig struct {
	MinVersion  uint16 `json:"min_version"`
	MaxVersion  uint16 `json:"max_version"`
	Certificate tls.Certificate
}

type CertificateFileConfig struct {
	PublicKeyFile  string `json:"public_key_file"`
	PrivateKeyFile string `json:"private_key_file"`
}

type ReverseProxyConfig struct {
	RemoteURL string `json:"remote_url"`
	Scheme    string `json:"scheme"`
	Host      string `json:"host"`
	Port      int    `json:"port"`
}
