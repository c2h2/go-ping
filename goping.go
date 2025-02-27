package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"sync"
	"syscall"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

// PingStats holds statistics for ICMP ping operations
type PingStats struct {
	Sent      int
	Received  int
	MinRTT    time.Duration
	MaxRTT    time.Duration
	TotalRTT  time.Duration
}

// getIPAndProtocol resolves the target host and determines the IP protocol
func getIPAndProtocol(host string) (*net.IPAddr, string, error) {
	ipAddr, err := net.ResolveIPAddr("ip", host)
	if err != nil {
		return nil, "", fmt.Errorf("failed to resolve host: %v", err)
	}
	if ipAddr.IP.To4() != nil {
		return ipAddr, "ip4:icmp", nil
	}
	return ipAddr, "ip6:icmp", nil
}

// protocolToInt maps protocol string to ICMP protocol number
func protocolToInt(protocol string) int {
	if protocol == "ip4:icmp" {
		return 1 // IPv4
	} else if protocol == "ip6:icmp" {
		return 58 // IPv6
	}
	return 0
}

// sendPing sends a single ICMP echo request and returns the round-trip time
func sendPing(conn *icmp.PacketConn, ipAddr *net.IPAddr, protocol string, seq int, timeout time.Duration) (time.Duration, error) {
	// Determine ICMP echo request type based on protocol
	var echoType icmp.Type
	if protocol == "ip4:icmp" {
		echoType = ipv4.ICMPTypeEcho
	} else {
		echoType = ipv6.ICMPTypeEchoRequest
	}

	// Create ICMP message
	message := icmp.Message{
		Type: echoType,
		Code: 0,
		Body: &icmp.Echo{
			ID:   os.Getpid() & 0xffff,
			Seq:  seq,
			Data: []byte("HELLO-R-U-THERE"),
		},
	}

	// Marshal the message
	bytes, err := message.Marshal(nil)
	if err != nil {
		return 0, err
	}

	// Send the ping
	start := time.Now()
	_, err = conn.WriteTo(bytes, ipAddr)
	if err != nil {
		return 0, err
	}

	// Set read deadline
	conn.SetReadDeadline(time.Now().Add(timeout))

	// Receive the reply
	reply := make([]byte, 1500)
	n, peer, err := conn.ReadFrom(reply)
	if err != nil {
		return 0, err
	}
	duration := time.Since(start)

	// Parse the reply
	msg, err := icmp.ParseMessage(protocolToInt(protocol), reply[:n])
	if err != nil {
		return 0, err
	}

	// Verify the reply is from the target
	if peer.String() != ipAddr.String() {
		return 0, fmt.Errorf("received reply from different peer")
	}

	// Check if it's an echo reply and matches our ID and sequence
	switch msg.Type {
	case ipv4.ICMPTypeEchoReply, ipv6.ICMPTypeEchoReply:
		if echo, ok := msg.Body.(*icmp.Echo); ok {
			if echo.ID == os.Getpid()&0xffff && echo.Seq == seq {
				return duration, nil
			}
		}
	}
	return 0, fmt.Errorf("invalid or mismatched reply")
}

// standardPing performs a standard ping with specified count and interval
func standardPing(host string, count int, interval, timeout time.Duration) {
	ipAddr, protocol, err := getIPAndProtocol(host)
	if err != nil {
		fmt.Println(err)
		return
	}

	// Create ICMP connection
	listenAddr := "0.0.0.0"
	if protocol == "ip6:icmp" {
		listenAddr = "::"
	}
	conn, err := icmp.ListenPacket(protocol, listenAddr)
	if err != nil {
		fmt.Printf("Failed to create ICMP connection: %v (may require root privileges)\n", err)
		return
	}
	defer conn.Close()

	// Initialize stats
	stats := PingStats{MinRTT: time.Duration(1<<63 - 1)}

	// Send pings sequentially
	for i := 0; i < count; i++ {
		rtt, err := sendPing(conn, ipAddr, protocol, i, timeout)
		if err == nil {
			stats.Received++
			if rtt < stats.MinRTT {
				stats.MinRTT = rtt
			}
			if rtt > stats.MaxRTT {
				stats.MaxRTT = rtt
			}
			stats.TotalRTT += rtt
			fmt.Printf("Reply from %s: seq=%d time=%.3f ms\n", host, i, float64(rtt)/float64(time.Millisecond))
		} else {
			fmt.Printf("Request timed out for seq=%d\n", i)
		}
		stats.Sent++
		if i < count-1 {
			time.Sleep(interval)
		}
	}

	// Print summary
	printStats(host, stats)
}

// floodPing performs a flood ping by sending pings concurrently
func floodPing(host string, count int, timeout time.Duration) {
	ipAddr, protocol, err := getIPAndProtocol(host)
	if err != nil {
		fmt.Println(err)
		return
	}

	// Create ICMP connection
	listenAddr := "0.0.0.0"
	if protocol == "ip6:icmp" {
		listenAddr = "::"
	}
	conn, err := icmp.ListenPacket(protocol, listenAddr)
	if err != nil {
		fmt.Printf("Failed to create ICMP connection: %v (may require root privileges)\n", err)
		return
	}
	defer conn.Close()

	// Initialize synchronization and results channel
	var wg sync.WaitGroup
	results := make(chan time.Duration, count)

	// Send pings concurrently
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(seq int) {
			defer wg.Done()
			rtt, err := sendPing(conn, ipAddr, protocol, seq, timeout)
			if err == nil {
				results <- rtt
			} else {
				results <- -1 // Indicate failure
			}
		}(i)
	}

	// Close results channel after all pings are done
	go func() {
		wg.Wait()
		close(results)
	}()

	// Collect results
	stats := PingStats{Sent: count, MinRTT: time.Duration(1<<63 - 1)}
	for rtt := range results {
		if rtt >= 0 {
			stats.Received++
			if rtt < stats.MinRTT {
				stats.MinRTT = rtt
			}
			if rtt > stats.MaxRTT {
				stats.MaxRTT = rtt
			}
			stats.TotalRTT += rtt
		}
	}

	// Print summary
	printStats(host, stats)
}

// tcpPing checks if a specified port is open on the target host
func tcpPing(host string, port int, timeout time.Duration) bool {
	addr := fmt.Sprintf("%s:%d", host, port)
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		// Check if the error is "connection refused" (port closed) or another issue
		if operr, ok := err.(*net.OpError); ok && operr.Err == syscall.ECONNREFUSED {
			return false
		}
		return false // Timeout or other error
	}
	conn.Close()
	return true
}

// printStats prints the ping statistics
func printStats(host string, stats PingStats) {
	fmt.Printf("\n--- %s ping statistics ---\n", host)
	loss := float64(stats.Sent-stats.Received) / float64(stats.Sent) * 100
	fmt.Printf("%d packets transmitted, %d received, %.2f%% packet loss\n", stats.Sent, stats.Received, loss)
	if stats.Received > 0 {
		avgRTT := stats.TotalRTT / time.Duration(stats.Received)
		fmt.Printf("rtt min/avg/max = %.3f/%.3f/%.3f ms\n",
			float64(stats.MinRTT)/float64(time.Millisecond),
			float64(avgRTT)/float64(time.Millisecond),
			float64(stats.MaxRTT)/float64(time.Millisecond))
	}
}

func main() {
	// Define command-line flags
	var (
		pingType = flag.String("type", "standard", "Type of ping: standard, flood, tcp")
		host     = flag.String("host", "", "Target host (required)")
		count    = flag.Int("count", 5, "Number of pings for standard and flood")
		interval = flag.Float64("interval", 1.0, "Interval between pings in seconds (standard ping)")
		port     = flag.Int("port", 80, "Port number for TCP ping")
		timeout  = flag.Float64("timeout", 1.0, "Timeout for each ping in seconds")
	)
	flag.Parse()

	// Validate host parameter
	if *host == "" {
		fmt.Println("Error: Target host is required")
		flag.Usage()
		os.Exit(1)
	}

	// Convert timeout and interval to time.Duration
	timeoutDuration := time.Duration(*timeout * float64(time.Second))
	intervalDuration := time.Duration(*interval * float64(time.Second))

	// Execute the requested ping type
	switch *pingType {
	case "standard":
		standardPing(*host, *count, intervalDuration, timeoutDuration)
	case "flood":
		floodPing(*host, *count, timeoutDuration)
	case "tcp":
		result := tcpPing(*host, *port, timeoutDuration)
		if result {
			fmt.Printf("TCP ping to %s:%d - Port is open\n", *host, *port)
		} else {
			fmt.Printf("TCP ping to %s:%d - Port is closed or unreachable\n", *host, *port)
		}
	default:
		fmt.Printf("Error: Invalid ping type '%s'. Use 'standard', 'flood', or 'tcp'\n", *pingType)
		flag.Usage()
		os.Exit(1)
	}
}
