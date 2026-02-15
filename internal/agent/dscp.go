// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package agent

import (
	"fmt"
	"net"
	"strings"
	"syscall"
)

// dscpValues mapeia nomes DSCP (RFC 2474/4594) para seus valores numéricos (6 bits).
// O valor é o DSCP code point, NÃO o byte TOS completo.
// Para setar no socket, usar dscpValue << 2 (pois TOS = DSCP << 2 + ECN).
var dscpValues = map[string]int{
	// Expedited Forwarding — VoIP, tempo real
	"EF": 46,

	// Assured Forwarding — classes 1-4, drop precedence 1-3
	"AF11": 10, "AF12": 12, "AF13": 14,
	"AF21": 18, "AF22": 20, "AF23": 22,
	"AF31": 26, "AF32": 28, "AF33": 30,
	"AF41": 34, "AF42": 36, "AF43": 38,

	// Class Selector (backward compatible com IP Precedence)
	"CS0": 0, "CS1": 8, "CS2": 16, "CS3": 24,
	"CS4": 32, "CS5": 40, "CS6": 48, "CS7": 56,
}

// ParseDSCP converte um nome DSCP (ex: "AF41", "EF") para o valor numérico.
// Retorna 0 e nil para string vazia (DSCP desabilitado).
func ParseDSCP(name string) (int, error) {
	name = strings.TrimSpace(strings.ToUpper(name))
	if name == "" {
		return 0, nil // desabilitado
	}

	val, ok := dscpValues[name]
	if !ok {
		return 0, fmt.Errorf("unknown DSCP value %q (valid: EF, AF11..AF43, CS0..CS7)", name)
	}
	return val, nil
}

// ApplyDSCP seta o campo TOS (DSCP) em uma conexão TCP.
// dscp é o valor do code point (0-63), que será shiftado para o byte TOS.
// Retorna nil se dscp == 0 (noop).
func ApplyDSCP(conn net.Conn, dscp int) error {
	if dscp == 0 {
		return nil
	}

	tcpConn, ok := conn.(*net.TCPConn)
	if !ok {
		return fmt.Errorf("cannot apply DSCP: conn is %T, not *net.TCPConn", conn)
	}

	rawConn, err := tcpConn.SyscallConn()
	if err != nil {
		return fmt.Errorf("getting raw conn for DSCP: %w", err)
	}

	// TOS byte = DSCP (6 bits) << 2 | ECN (2 bits, leave as 0)
	tosValue := dscp << 2

	var sysErr error
	if err := rawConn.Control(func(fd uintptr) {
		sysErr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, syscall.IP_TOS, tosValue)
	}); err != nil {
		return fmt.Errorf("control fd for DSCP: %w", err)
	}
	if sysErr != nil {
		return fmt.Errorf("setsockopt IP_TOS=%d: %w", tosValue, sysErr)
	}

	return nil
}
