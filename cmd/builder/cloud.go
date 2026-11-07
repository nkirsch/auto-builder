package main

import (
	"net/netip"
	"slices"
	"strings"
)

// cloudNets are the ranges cloud providers rent to their customers
// (cloudranges.go, from go run ./cmd/cloudranges > cmd/builder/cloudranges.go), sorted by start address
// and not overlapping. A browser there is a headless scanner, not a reader.
var cloudNets = loadCloudNets(map[string]string{"Google Cloud": googleCloudRanges, "AWS": awsRanges})

type cloudNet struct {
	p        netip.Prefix
	provider string
}

func loadCloudNets(lists map[string]string) []cloudNet {
	var ns []cloudNet
	for provider, list := range lists {
		for _, s := range strings.Fields(list) {
			if p, err := netip.ParsePrefix(s); err == nil {
				ns = append(ns, cloudNet{p.Masked(), provider})
			}
		}
	}
	slices.SortFunc(ns, func(x, y cloudNet) int { return x.p.Addr().Compare(y.p.Addr()) })
	return ns
}

// cloudProvider names the cloud whose range holds ip, or "".
func cloudProvider(ip string) string {
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return ""
	}
	a = a.Unmap()
	// The last range starting at or before a is the only one that can hold it.
	i, _ := slices.BinarySearchFunc(cloudNets, a, func(n cloudNet, a netip.Addr) int {
		if n.p.Addr().Compare(a) <= 0 {
			return -1
		}
		return 1
	})
	if i > 0 && cloudNets[i-1].p.Contains(a) {
		return cloudNets[i-1].provider
	}
	return ""
}
