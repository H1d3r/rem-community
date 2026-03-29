package utils

import "strconv"

func NewProxies(options map[string]string) *Proxies {
	return &Proxies{
		Name:           options["name"],
		Type:           options["type"],
		Server:         options["server"],
		Port:           StringToInt(options["port"]),
		Username:       options["username"],
		Password:       options["password"],
		Udp:            true,
		Tls:            false,
		SkipCertVerify: true,
		Cipher:         options["cipher"],
	}
}

type Proxies struct {
	Name           string `yaml:"name,omitempty"`
	Type           string `yaml:"type"`
	Server         string `yaml:"server"`
	Port           int    `yaml:"port"`
	Username       string `yaml:"username,omitempty"`
	Password       string `yaml:"password,omitempty"`
	Udp            bool   `yaml:"udp"`
	Tls            bool   `yaml:"tls"`
	SkipCertVerify bool   `yaml:"skip-cert-verify"`
	Cipher         string `yaml:"cipher,omitempty"`
}

// StringToInt 将字符串转换为整数，如果转换失败则返回0
func StringToInt(s string) int {
	if s == "" {
		return 0
	}
	i, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return i
}

type ProxyGroup struct {
	Name    string   `yaml:"name"`
	Type    string   `yaml:"type"`
	Proxies []string `yaml:"proxies"`
}

type ClashConfig struct {
	Proxies     []*Proxies    `yaml:"proxies"`
	Mode        string        `yaml:"mode"`
	Rules       []string      `yaml:"rules"`
	ProxyGroups []*ProxyGroup `yaml:"proxy-groups"`
}
