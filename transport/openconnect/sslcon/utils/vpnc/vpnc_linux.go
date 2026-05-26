package vpnc

import (
	"fmt"
	"net"
	"os/exec"
	"strings"
	"time"

	"github.com/vishvananda/netlink"
	"github.com/WarrDoge/sslcon/base"
	"github.com/WarrDoge/sslcon/session"
	"github.com/WarrDoge/sslcon/utils"
)

var (
	localInterface netlink.Link
	iface          netlink.Link
)

func ConfigInterface(cSess *session.ConnSession) error {
	var err error
	iface, err = netlink.LinkByName(cSess.TunName)
	if err != nil {
		return err
	}
	// ip address
	_ = netlink.LinkSetUp(iface)
	_ = netlink.LinkSetMulticastOff(iface)

	addr, _ := netlink.ParseAddr(utils.IpMask2CIDR(cSess.VPNAddress, cSess.VPNMask))
	err = netlink.AddrAdd(iface, addr)

	return err
}

func SetRoutes(cSess *session.ConnSession) error {
	// routes
	dst, _ := netlink.ParseIPNet(cSess.ServerAddress + "/32")
	gateway := net.ParseIP(base.LocalInterface.Gateway)

	ifaceIndex := iface.Attrs().Index
	localInterfaceIndex := localInterface.Attrs().Index

	route := netlink.Route{LinkIndex: localInterfaceIndex, Dst: dst, Gw: gateway}
	err := netlink.RouteAdd(&route)
	if err != nil {
		if !strings.HasSuffix(err.Error(), "exists") {
			return routingError(dst, err)
		}
	}

	splitInclude := cSess.SplitInclude
	if len(base.Cfg.SplitRoutes) > 0 {
		splitInclude = append([]string(nil), base.Cfg.SplitRoutes...)
	}

	if len(splitInclude) == 0 {
		splitInclude = append(splitInclude, "0.0.0.0/0.0.0.0")

		// Full-tunnel mode: reset the default route priority, for example OpenWrt often defaults to priority 0.
		zero, _ := netlink.ParseIPNet("0.0.0.0/0")
		delAllRoute(&netlink.Route{LinkIndex: localInterfaceIndex, Dst: zero})
		_ = netlink.RouteAdd(&netlink.Route{LinkIndex: localInterfaceIndex, Dst: zero, Gw: gateway, Priority: 10})
	}
	cSess.SplitInclude = splitInclude

	// With domain-based includes, excluding the IP of a specific subdomain from a top-level domain match is not supported.
	for _, routeSpec := range cSess.SplitInclude {
		cidr, routeErr := routeToCIDR(routeSpec)
		if routeErr != nil {
			return routeErr
		}
		dst, _ = netlink.ParseIPNet(cidr)
		route = netlink.Route{LinkIndex: ifaceIndex, Dst: dst, Priority: 6}
		err = netlink.RouteAdd(&route)
		if err != nil {
			if !strings.HasSuffix(err.Error(), "exists") {
				return routingError(dst, err)
			}
		}
	}

	// Allow a route to be excluded even if it falls within SplitInclude.
	if len(cSess.SplitExclude) > 0 {
		for _, routeSpec := range cSess.SplitExclude {
			cidr, routeErr := routeToCIDR(routeSpec)
			if routeErr != nil {
				return routeErr
			}
			dst, _ = netlink.ParseIPNet(cidr)
			route = netlink.Route{LinkIndex: localInterfaceIndex, Dst: dst, Gw: gateway, Priority: 5}
			err = netlink.RouteAdd(&route)
			if err != nil {
				if !strings.HasSuffix(err.Error(), "exists") {
					return routingError(dst, err)
				}
			}
		}
	}

	if len(cSess.DNS) > 0 {
		setDNS(cSess)
	}

	return nil
}

func ResetRoutes(cSess *session.ConnSession) {
	// routes
	localInterfaceIndex := localInterface.Attrs().Index

	for _, ipMask := range cSess.SplitInclude {
		if ipMask == "0.0.0.0/0.0.0.0" {
			// Restore the default route priority.
			zero, _ := netlink.ParseIPNet("0.0.0.0/0")
			gateway := net.ParseIP(base.LocalInterface.Gateway)
			_ = netlink.RouteDel(&netlink.Route{LinkIndex: localInterfaceIndex, Dst: zero})
			_ = netlink.RouteAdd(&netlink.Route{LinkIndex: localInterfaceIndex, Dst: zero, Gw: gateway})
			break
		}
	}

	dst, _ := netlink.ParseIPNet(cSess.ServerAddress + "/32")
	_ = netlink.RouteDel(&netlink.Route{LinkIndex: localInterfaceIndex, Dst: dst})

	if len(cSess.SplitExclude) > 0 {
		for _, routeSpec := range cSess.SplitExclude {
			cidr, err := routeToCIDR(routeSpec)
			if err != nil {
				continue
			}
			dst, _ = netlink.ParseIPNet(cidr)
			_ = netlink.RouteDel(&netlink.Route{LinkIndex: localInterfaceIndex, Dst: dst})
		}
	}

	if len(cSess.DynamicSplitExcludeDomains) > 0 {
		cSess.DynamicSplitExcludeResolved.Range(func(_, value any) bool {
			ips := value.([]string)
			for _, ip := range ips {
				dst, _ = netlink.ParseIPNet(ip + "/32")
				_ = netlink.RouteDel(&netlink.Route{LinkIndex: localInterfaceIndex, Dst: dst})
			}

			return true
		})
	}

	if len(cSess.DNS) > 0 {
		restoreDNS(cSess)
	}
}

func DynamicAddIncludeRoutes(ips []string) {
	ifaceIndex := iface.Attrs().Index

	for _, ip := range ips {
		dst, _ := netlink.ParseIPNet(ip + "/32")
		route := netlink.Route{LinkIndex: ifaceIndex, Dst: dst, Priority: 6}
		_ = netlink.RouteAdd(&route)
	}
}

func DynamicAddExcludeRoutes(ips []string) {
	localInterfaceIndex := localInterface.Attrs().Index
	gateway := net.ParseIP(base.LocalInterface.Gateway)

	for _, ip := range ips {
		dst, _ := netlink.ParseIPNet(ip + "/32")
		route := netlink.Route{LinkIndex: localInterfaceIndex, Dst: dst, Gw: gateway, Priority: 5}
		_ = netlink.RouteAdd(&route)
	}
}

func GetLocalInterface() error {

	// just for default route
	routes, err := netlink.RouteGet(net.ParseIP("8.8.8.8"))
	if len(routes) > 0 {
		route := routes[0]
		localInterface, err = netlink.LinkByIndex(route.LinkIndex)
		if err != nil {
			return err
		}
		base.LocalInterface.Name = localInterface.Attrs().Name
		base.LocalInterface.Ip4 = route.Src.String()
		base.LocalInterface.Gateway = route.Gw.String()
		base.LocalInterface.Mac = localInterface.Attrs().HardwareAddr.String()

		base.Info("GetLocalInterface:", fmt.Sprintf("%+v", *base.LocalInterface))
		return nil
	}
	return err
}

func delAllRoute(route *netlink.Route) {
	err := netlink.RouteDel(route)
	if err != nil {
		return
	}
	delAllRoute(route)
}

func routingError(dst *net.IPNet, err error) error {
	return fmt.Errorf("routing error: %s %s", dst.String(), err)
}

func setDNS(cSess *session.ConnSession) {
	// dns
	if len(cSess.DNS) > 0 {
		// DNS must go through the VPN when using dynamic domain routes so the traffic can be inspected.
		if len(cSess.DynamicSplitIncludeDomains) > 0 {
			DynamicAddIncludeRoutes(cSess.DNS)
		}

		// Some cloud systems rewrite /etc/resolv.conf while routes are being configured, so delay for two seconds.
		go func() {
			utils.CopyFile("/tmp/resolv.conf.bak", "/etc/resolv.conf")

			var dnsString string
			for _, dns := range cSess.DNS {
				dnsString += fmt.Sprintf("nameserver %s\n", dns)
			}
			domains := NormalizeDNSDomains(base.Cfg.DNSDomains)
			if len(domains) > 0 {
				dnsString += "search " + strings.Join(domains, " ") + "\n"
			}
			time.Sleep(2 * time.Second)
			// OpenWrt may append 127.0.0.1 at the bottom, which affects resolution for entries above it.
			err := utils.NewRecord("/etc/resolv.conf").Write(dnsString, false)
			if err != nil {
				base.Error("set DNS failed")
			}
		}()
	}
}

func restoreDNS(cSess *session.ConnSession) {
	// dns
	// If the process crashes, resolv.conf may not be restored and networking can remain broken until reboot.
	if len(cSess.DNS) > 0 {
		utils.CopyFile("/etc/resolv.conf", "/tmp/resolv.conf.bak")
	}
}

func execCmd(cmdStrs []string) error {
	for _, cmdStr := range cmdStrs {
		cmd := exec.Command("sh", "-c", cmdStr)
		stdoutStderr, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s %s %s", err, cmd.String(), string(stdoutStderr))
		}
	}
	return nil
}
