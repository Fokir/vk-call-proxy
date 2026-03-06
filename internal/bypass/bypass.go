package bypass

import (
	"net"
	"strings"
)

// Matcher checks whether a host should bypass the VPN tunnel.
type Matcher struct {
	domains map[string]struct{} // exact domains
	suffixes []string           // domain suffixes (with leading dot)
}

// New creates a Matcher from a list of domain patterns.
// Each pattern is either an exact domain or a suffix pattern starting with ".".
// A bare domain "example.com" matches both "example.com" and "*.example.com".
func New(patterns []string) *Matcher {
	m := &Matcher{
		domains: make(map[string]struct{}),
	}
	for _, p := range patterns {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" {
			continue
		}
		m.domains[p] = struct{}{}
		if !strings.HasPrefix(p, ".") {
			m.suffixes = append(m.suffixes, "."+p)
		} else {
			m.suffixes = append(m.suffixes, p)
		}
	}
	return m
}

// Match returns true if the given host:port address should bypass the VPN.
func (m *Matcher) Match(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	host = strings.ToLower(host)

	// IP addresses are never bypassed by domain matcher.
	if net.ParseIP(host) != nil {
		return false
	}

	if _, ok := m.domains[host]; ok {
		return true
	}
	for _, suffix := range m.suffixes {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	return false
}

// DefaultRussianServices returns bypass patterns for popular Russian services.
func DefaultRussianServices() []string {
	return []string{
		// VK
		"vk.com",
		"vk.me",
		"vk.cc",
		"vk.ru",
		"vkontakte.ru",
		"userapi.com",
		"vk-cdn.net",
		"vkuser.net",
		"vkuseraudio.net",
		"vkuservideo.net",
		"vk-portal.net",
		"vk-apps.com",
		"vkmix.com",

		// VK Video
		"vkvideo.ru",
		"vkvideo.net",

		// Yandex (all services)
		"yandex.ru",
		"yandex.net",
		"yandex.com",
		"yandex.by",
		"yandex.kz",
		"yandex.ua",
		"ya.ru",
		"yastatic.net",
		"yastat.net",
		"yandex-team.ru",
		"yandexcloud.net",
		"yandexdataschool.ru",
		"yadisk.cc",
		"yandexmetrica.com",
		"yandex.cloud",
		"yandex.st",
		"yandexadexchange.net",
		"yandex-ad.cn",
		"yandex.fr",

		// Yandex Go (taxi)
		"taxi.yandex.ru",
		"tc.yandex.ru",

		// Mobile operators
		"mts.ru",
		"megafon.ru",
		"beeline.ru",
		"tele2.ru",
		"t2mobile.ru",
		"rt.ru",
		"rostelecom.ru",
		"yota.ru",
		"tinkoffmobile.ru",
		"sberbank.ru",
		"sbermobile.ru",

		// Rutube
		"rutube.ru",

		// Okko
		"okko.tv",

		// Ivi
		"ivi.ru",
		"ivi.tv",

		// Pochta Rossii
		"pochta.ru",
		"russianpost.ru",

		// GIS ZhKH
		"dom.gosuslugi.ru",
		"gis-zkh.ru",

		// Gosuslugi
		"gosuslugi.ru",
		"esia.gosuslugi.ru",
		"pfr.gov.ru",
		"nalog.gov.ru",

		// Banks
		"sberbank.ru",
		"online.sberbank.ru",
		"tinkoff.ru",
		"vtb.ru",
		"alfabank.ru",
		"raiffeisen.ru",
		"open.ru",
		"sovcombank.ru",
		"halvacard.ru",
		"rosbank.ru",
		"psbank.ru",
		"gazprombank.ru",
		"rshb.ru",
		"pochtabank.ru",
		"unicreditbank.ru",
		"mtsbank.ru",
		"yoomoney.ru",
		"homecredit.ru",
	}
}
