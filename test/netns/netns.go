package netns

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
)

type Namespace struct {
	name string
}

func Create(name string) (*Namespace, error) {
	if err := exec.Command("ip", "netns", "add", name).Run(); err != nil {
		return nil, fmt.Errorf("create netns %s: %w", name, err)
	}
	return &Namespace{name: name}, nil
}

func (n *Namespace) Delete() error {
	return exec.Command("ip", "netns", "delete", n.name).Run()
}

func (n *Namespace) Exec(cmd string, args ...string) error {
	fullArgs := append([]string{"netns", "exec", n.name}, cmd)
	fullArgs = append(fullArgs, args...)
	return exec.Command("ip", fullArgs...).Run()
}

func (n *Namespace) ExecOutput(cmd string, args ...string) (string, error) {
	fullArgs := append([]string{"netns", "exec", n.name}, cmd)
	fullArgs = append(fullArgs, args...)
	out, err := exec.Command("ip", fullArgs...).CombinedOutput()
	return string(out), err
}

type Veth struct {
	name1 string
	name2 string
	ns1   *Namespace
	ns2   *Namespace
}

func CreateVeth(ns1, ns2 *Namespace, name1, name2 string) (*Veth, error) {
	veth1 := "veth-" + name1
	veth2 := "veth-" + name2

	if err := exec.Command("ip", "link", "add", veth1, "type", "veth", "peer", "name", veth2).Run(); err != nil {
		return nil, fmt.Errorf("create veth pair: %w", err)
	}

	if err := exec.Command("ip", "link", "set", veth1, "netns", ns1.name).Run(); err != nil {
		exec.Command("ip", "link", "delete", veth1).Run()
		return nil, fmt.Errorf("move veth1 to ns1: %w", err)
	}

	if err := exec.Command("ip", "link", "set", veth2, "netns", ns2.name).Run(); err != nil {
		exec.Command("ip", "link", "delete", veth1).Run()
		return nil, fmt.Errorf("move veth2 to ns2: %w", err)
	}

	return &Veth{
		name1: veth1,
		name2: veth2,
		ns1:   ns1,
		ns2:   ns2,
	}, nil
}

func (v *Veth) ConfigureAddrs(addr1, addr2 string) error {
	if err := v.ns1.Exec("ip", "addr", "add", addr1, "dev", v.name1); err != nil {
		return fmt.Errorf("configure addr1: %w", err)
	}
	if err := v.ns2.Exec("ip", "addr", "add", addr2, "dev", v.name2); err != nil {
		return fmt.Errorf("configure addr2: %w", err)
	}

	if err := v.ns1.Exec("ip", "link", "set", v.name1, "up"); err != nil {
		return fmt.Errorf("bring up veth1: %w", err)
	}
	if err := v.ns2.Exec("ip", "link", "set", v.name2, "up"); err != nil {
		return fmt.Errorf("bring up veth2: %w", err)
	}

	return nil
}

func (v *Veth) Delete() error {
	return exec.Command("ip", "link", "delete", "veth-"+strings.TrimPrefix(v.name1, "veth-")).Run()
}

func (n *Namespace) GetInterfaceIP(ifname string) (string, error) {
	output, err := n.ExecOutput("ip", "addr", "show", ifname)
	if err != nil {
		return "", err
	}

	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "inet ") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				return parts[1], nil
			}
		}
	}

	return "", fmt.Errorf("no inet address found for %s", ifname)
}

func (n *Namespace) ListenTCP(addr string) (net.Listener, error) {
	lc := &net.ListenConfig{
		Control: func(network, address string, c interface{}) error {
			return setNetns(c, n.name)
		},
	}
	return lc.Listen(nil, "tcp", addr)
}

func setNetns(c interface{}, nsname string) error {
	ns, err := exec.Command("ip", "netns", "identify").Output()
	if err == nil && strings.TrimSpace(string(ns)) == nsname {
		return nil
	}

	if err := exec.Command("ip", "netns", "exec", nsname, "true").Run(); err != nil {
		return fmt.Errorf("verify netns exists: %w", err)
	}
	return nil
}
