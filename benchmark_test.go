package quic

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"io"
	"net"
	"reflect"
	"time"
	"unsafe"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	"github.com/lucas-clemente/quic-go/crypto"
	"github.com/lucas-clemente/quic-go/handshake"
	"github.com/lucas-clemente/quic-go/protocol"
	"github.com/lucas-clemente/quic-go/utils"
)

type linkedConnection struct {
	other *Session
}

func (c *linkedConnection) write(p []byte) error {
	packet := getPacketBuffer()
	packet = packet[:len(p)]
	copy(packet, p)

	go func() {
		r := bytes.NewReader(packet)
		hdr, err := parsePublicHeader(r)
		if err != nil {
			Expect(err).NotTo(HaveOccurred())
		}
		hdr.Raw = packet[:len(packet)-r.Len()]

		c.other.handlePacket(nil, hdr, packet[len(packet)-r.Len():])

	}()
	return nil
}

func (*linkedConnection) setCurrentRemoteAddr(addr interface{}) {}
func (*linkedConnection) IP() net.IP                            { return nil }

func setAEAD(cs *handshake.CryptoSetup, aead crypto.AEAD) {
	*(*bool)(unsafe.Pointer(reflect.ValueOf(cs).Elem().FieldByName("receivedForwardSecurePacket").UnsafeAddr())) = true
	*(*crypto.AEAD)(unsafe.Pointer(reflect.ValueOf(cs).Elem().FieldByName("forwardSecureAEAD").UnsafeAddr())) = aead
}

func setFlowControlParameters(mgr *handshake.ConnectionParametersManager) {
	sfcw := make([]byte, 4)
	cfcw := make([]byte, 4)
	binary.LittleEndian.PutUint32(sfcw, uint32(protocol.ReceiveStreamFlowControlWindow))
	binary.LittleEndian.PutUint32(cfcw, uint32(protocol.ReceiveConnectionFlowControlWindow))
	mgr.SetFromMap(map[handshake.Tag][]byte{
		handshake.TagSFCW: sfcw,
		handshake.TagCFCW: cfcw,
	})
}

var _ = FDescribe("Benchmarks", func() {
	// utils.SetLogLevel(utils.LogLevelDebug)
	// version := protocol.SupportedVersions[len(protocol.SupportedVersions)-1]
	version := protocol.Version34
	connID := protocol.ConnectionID(42)
	dataLen := 100 /* MB */ * (1 << 20)
	data := make([]byte, dataLen)

	Measure("two linked sessions", func(b Benchmarker) {
		c1 := &linkedConnection{}
		session1I, err := newSession(c1, version, connID, nil, func(*Session, utils.Stream) {}, func(id protocol.ConnectionID) {})
		if err != nil {
			Expect(err).NotTo(HaveOccurred())
		}
		session1 := session1I.(*Session)

		c2 := &linkedConnection{other: session1}
		session2I, err := newSession(c2, version, connID, nil, func(*Session, utils.Stream) {}, func(id protocol.ConnectionID) {})
		if err != nil {
			Expect(err).NotTo(HaveOccurred())
		}
		session2 := session2I.(*Session)
		c1.other = session2

		key := make([]byte, 16)
		iv := make([]byte, 4)
		rand.Read(key)
		rand.Read(iv)
		aead, err := crypto.NewAEADAESGCM(key, key, iv, iv)
		Expect(err).NotTo(HaveOccurred())
		setAEAD(session1.cryptoSetup, aead)
		setAEAD(session2.cryptoSetup, aead)

		setFlowControlParameters(session1.connectionParametersManager)
		setFlowControlParameters(session2.connectionParametersManager)

		go session1.run()
		go session2.run()

		s1stream, err := session1.OpenStream(5)
		Expect(err).NotTo(HaveOccurred())
		s2stream, err := session2.OpenStream(5)
		Expect(err).NotTo(HaveOccurred())

		done := make(chan struct{})
		go func() {
			defer GinkgoRecover()
			buf := make([]byte, 1024)
			dataRead := 0
			for dataRead < dataLen {
				n, err := s2stream.Read(buf)
				Expect(err).NotTo(HaveOccurred())
				dataRead += n
			}
			done <- struct{}{}
		}()

		time.Sleep(time.Millisecond)
		runtime := b.Time("transfer time", func() {
			_, err := io.Copy(s1stream, bytes.NewReader(data))
			Expect(err).NotTo(HaveOccurred())
			<-done
		})

		session1.Close(nil)
		session2.Close(nil)
		time.Sleep(time.Millisecond)

		b.RecordValue("transfer rate [MB/s]", float64(dataLen)/1e6/runtime.Seconds())
	}, 8)
})
