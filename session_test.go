package quic

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	"github.com/lucas-clemente/quic-go/ackhandlerlegacy"
	"github.com/lucas-clemente/quic-go/crypto"
	"github.com/lucas-clemente/quic-go/frames"
	"github.com/lucas-clemente/quic-go/handshake"
	"github.com/lucas-clemente/quic-go/protocol"
	"github.com/lucas-clemente/quic-go/qerr"
	"github.com/lucas-clemente/quic-go/testdata"
	"github.com/lucas-clemente/quic-go/utils"
)

type mockConnection struct {
	written [][]byte
}

func (m *mockConnection) write(p []byte) error {
	b := make([]byte, len(p))
	copy(b, p)
	m.written = append(m.written, b)
	return nil
}

func (*mockConnection) setCurrentRemoteAddr(addr interface{}) {}
func (*mockConnection) IP() net.IP                            { return nil }

type mockUnpacker struct{}

func (m *mockUnpacker) Unpack(publicHeaderBinary []byte, hdr *publicHeader, data []byte) (*unpackedPacket, error) {
	return &unpackedPacket{
		entropyBit: false,
		frames:     nil,
	}, nil
}

type mockSentPacketHandler struct {
	retransmissionQueue []*ackhandlerlegacy.Packet
}

func (h *mockSentPacketHandler) SentPacket(packet *ackhandlerlegacy.Packet) error { return nil }
func (h *mockSentPacketHandler) ReceivedAck(ackFrame *frames.AckFrame, withPacketNumber protocol.PacketNumber) error {
	return nil
}
func (h *mockSentPacketHandler) BytesInFlight() protocol.ByteCount      { return 0 }
func (h *mockSentPacketHandler) GetLargestAcked() protocol.PacketNumber { return 1 }
func (h *mockSentPacketHandler) GetStopWaitingFrame() *frames.StopWaitingFrame {
	panic("not implemented")
}
func (h *mockSentPacketHandler) CongestionAllowsSending() bool { return true }
func (h *mockSentPacketHandler) CheckForError() error          { return nil }
func (h *mockSentPacketHandler) TimeOfFirstRTO() time.Time     { panic("not implemented") }

func (h *mockSentPacketHandler) ProbablyHasPacketForRetransmission() bool {
	return len(h.retransmissionQueue) > 0
}

func (h *mockSentPacketHandler) DequeuePacketForRetransmission() *ackhandlerlegacy.Packet {
	if len(h.retransmissionQueue) > 0 {
		packet := h.retransmissionQueue[0]
		h.retransmissionQueue = h.retransmissionQueue[1:]
		return packet
	}
	return nil
}

func newMockSentPacketHandler() ackhandlerlegacy.SentPacketHandler {
	return &mockSentPacketHandler{}
}

var _ = Describe("Session", func() {
	var (
		session              *Session
		streamCallbackCalled bool
		closeCallbackCalled  bool
		conn                 *mockConnection
	)

	for _, versionLoop := range []protocol.VersionNumber{protocol.Version33, protocol.Version34} {
		version := versionLoop

		Context(fmt.Sprintf("with quic version %d", version), func() {

			BeforeEach(func() {
				conn = &mockConnection{}
				streamCallbackCalled = false
				closeCallbackCalled = false

				signer, err := crypto.NewProofSource(testdata.GetTLSConfig())
				Expect(err).ToNot(HaveOccurred())
				kex, err := crypto.NewCurve25519KEX()
				Expect(err).NotTo(HaveOccurred())
				scfg, err := handshake.NewServerConfig(kex, signer)
				Expect(err).NotTo(HaveOccurred())
				pSession, err := newSession(
					conn,
					version,
					0,
					scfg,
					func(*Session, utils.Stream) { streamCallbackCalled = true },
					func(protocol.ConnectionID) { closeCallbackCalled = true },
				)
				Expect(err).NotTo(HaveOccurred())
				session = pSession.(*Session)
				Expect(session.streams).To(HaveLen(1)) // Crypto stream
			})

			Context("when handling stream frames", func() {
				It("makes new streams", func() {
					session.handleStreamFrame(&frames.StreamFrame{
						StreamID: 5,
						Data:     []byte{0xde, 0xca, 0xfb, 0xad},
					})
					Expect(session.streams).To(HaveLen(2))
					Expect(streamCallbackCalled).To(BeTrue())
					p := make([]byte, 4)
					_, err := session.streams[5].Read(p)
					Expect(err).ToNot(HaveOccurred())
					Expect(p).To(Equal([]byte{0xde, 0xca, 0xfb, 0xad}))
				})

				It("rejects streams with even StreamIDs", func() {
					err := session.handleStreamFrame(&frames.StreamFrame{
						StreamID: 4,
						Data:     []byte{0xde, 0xca, 0xfb, 0xad},
					})
					Expect(err).To(MatchError(qerr.InvalidStreamID))
				})

				It("does not reject existing streams with even StreamIDs", func() {
					_, err := session.OpenStream(4)
					Expect(err).ToNot(HaveOccurred())
					err = session.handleStreamFrame(&frames.StreamFrame{
						StreamID: 4,
						Data:     []byte{0xde, 0xca, 0xfb, 0xad},
					})
					Expect(err).ToNot(HaveOccurred())
				})

				It("handles existing streams", func() {
					session.handleStreamFrame(&frames.StreamFrame{
						StreamID: 5,
						Data:     []byte{0xde, 0xca},
					})
					Expect(session.streams).To(HaveLen(2))
					Expect(streamCallbackCalled).To(BeTrue())
					session.handleStreamFrame(&frames.StreamFrame{
						StreamID: 5,
						Offset:   2,
						Data:     []byte{0xfb, 0xad},
					})
					Expect(session.streams).To(HaveLen(2))
					p := make([]byte, 4)
					_, err := session.streams[5].Read(p)
					Expect(err).ToNot(HaveOccurred())
					Expect(p).To(Equal([]byte{0xde, 0xca, 0xfb, 0xad}))
				})

				It("does not delete streams with Close()", func() {
					str, err := session.OpenStream(5)
					Expect(err).ToNot(HaveOccurred())
					str.Close()
					session.garbageCollectStreams()
					Expect(session.streams).To(HaveLen(2))
					Expect(session.streams[5]).ToNot(BeNil())
				})

				It("does not delete streams with FIN bit", func() {
					session.handleStreamFrame(&frames.StreamFrame{
						StreamID: 5,
						Data:     []byte{0xde, 0xca, 0xfb, 0xad},
						FinBit:   true,
					})
					Expect(session.streams).To(HaveLen(2))
					Expect(session.streams[5]).ToNot(BeNil())
					Expect(streamCallbackCalled).To(BeTrue())
					p := make([]byte, 4)
					_, err := session.streams[5].Read(p)
					Expect(err).To(MatchError(io.EOF))
					Expect(p).To(Equal([]byte{0xde, 0xca, 0xfb, 0xad}))
					session.garbageCollectStreams()
					Expect(session.streams).To(HaveLen(2))
					Expect(session.streams[5]).ToNot(BeNil())
				})

				It("deletes streams with FIN bit & close", func() {
					session.handleStreamFrame(&frames.StreamFrame{
						StreamID: 5,
						Data:     []byte{0xde, 0xca, 0xfb, 0xad},
						FinBit:   true,
					})
					Expect(session.streams).To(HaveLen(2))
					Expect(session.streams[5]).ToNot(BeNil())
					Expect(streamCallbackCalled).To(BeTrue())
					p := make([]byte, 4)
					_, err := session.streams[5].Read(p)
					Expect(err).To(MatchError(io.EOF))
					Expect(p).To(Equal([]byte{0xde, 0xca, 0xfb, 0xad}))
					session.garbageCollectStreams()
					Expect(session.streams).To(HaveLen(2))
					Expect(session.streams[5]).ToNot(BeNil())
					// We still need to close the stream locally
					session.streams[5].Close()
					// ... and simulate that we actually the FIN
					session.streams[5].sentFin()
					session.garbageCollectStreams()
					Expect(session.streams).To(HaveLen(2))
					Expect(session.streams[5]).To(BeNil())
					// flow controller should have been notified
					_, err = session.flowControlManager.SendWindowSize(5)
					Expect(err).To(MatchError("Error accessing the flowController map."))
				})

				It("closes streams with error", func() {
					testErr := errors.New("test")
					session.handleStreamFrame(&frames.StreamFrame{
						StreamID: 5,
						Data:     []byte{0xde, 0xca, 0xfb, 0xad},
					})
					Expect(session.streams).To(HaveLen(2))
					Expect(session.streams[5]).ToNot(BeNil())
					Expect(streamCallbackCalled).To(BeTrue())
					p := make([]byte, 4)
					_, err := session.streams[5].Read(p)
					Expect(err).ToNot(HaveOccurred())
					session.closeStreamsWithError(testErr)
					_, err = session.streams[5].Read(p)
					Expect(err).To(MatchError(testErr))
					session.garbageCollectStreams()
					Expect(session.streams).To(HaveLen(2))
					Expect(session.streams[5]).To(BeNil())
				})

				It("closes empty streams with error", func() {
					testErr := errors.New("test")
					session.newStreamImpl(5)
					Expect(session.streams).To(HaveLen(2))
					Expect(session.streams[5]).ToNot(BeNil())
					session.closeStreamsWithError(testErr)
					_, err := session.streams[5].Read([]byte{0})
					Expect(err).To(MatchError(testErr))
					session.garbageCollectStreams()
					Expect(session.streams).To(HaveLen(2))
					Expect(session.streams[5]).To(BeNil())
				})

				It("informs the FlowControlManager about new streams", func() {
					// since the stream doesn't yet exist, this will throw an error
					err := session.flowControlManager.UpdateHighestReceived(5, 1000)
					Expect(err).To(HaveOccurred())
					session.newStreamImpl(5)
					err = session.flowControlManager.UpdateHighestReceived(5, 2000)
					Expect(err).ToNot(HaveOccurred())
				})

				It("ignores streams that existed previously", func() {
					session.handleStreamFrame(&frames.StreamFrame{
						StreamID: 5,
						Data:     []byte{},
						FinBit:   true,
					})
					_, err := session.streams[5].Read([]byte{0})
					Expect(err).To(MatchError(io.EOF))
					session.streams[5].Close()
					session.streams[5].sentFin()
					session.garbageCollectStreams()
					err = session.handleStreamFrame(&frames.StreamFrame{
						StreamID: 5,
						Data:     []byte{},
					})
					Expect(err).To(BeNil())
				})
			})

			Context("handling RST_STREAM frames", func() {
				It("closes the receiving streams for writing and reading", func() {
					s, err := session.OpenStream(5)
					Expect(err).ToNot(HaveOccurred())
					err = session.handleRstStreamFrame(&frames.RstStreamFrame{
						StreamID:  5,
						ErrorCode: 42,
					})
					Expect(err).ToNot(HaveOccurred())
					n, err := s.Write([]byte{0})
					Expect(n).To(BeZero())
					Expect(err).To(MatchError("RST_STREAM received with code 42"))
					n, err = s.Read([]byte{0})
					Expect(n).To(BeZero())
					Expect(err).To(MatchError("RST_STREAM received with code 42"))
				})

				It("errors when the stream is not known", func() {
					err := session.handleRstStreamFrame(&frames.RstStreamFrame{
						StreamID:  5,
						ErrorCode: 42,
					})
					Expect(err).To(MatchError(errRstStreamOnInvalidStream))
				})

				It("ignores the error when the stream is not known", func() {
					err := session.handleFrames([]frames.Frame{&frames.RstStreamFrame{
						StreamID:  5,
						ErrorCode: 42,
					}})
					Expect(err).NotTo(HaveOccurred())
				})
			})

			Context("handling WINDOW_UPDATE frames", func() {
				It("updates the Flow Control Window of a stream", func() {
					_, err := session.OpenStream(5)
					Expect(err).ToNot(HaveOccurred())
					err = session.handleWindowUpdateFrame(&frames.WindowUpdateFrame{
						StreamID:   5,
						ByteOffset: 100,
					})
					Expect(err).ToNot(HaveOccurred())
					Expect(session.flowControlManager.SendWindowSize(5)).To(Equal(protocol.ByteCount(100)))
				})

				It("updates the Flow Control Window of the connection", func() {
					err := session.handleWindowUpdateFrame(&frames.WindowUpdateFrame{
						StreamID:   0,
						ByteOffset: 0x800000,
					})
					Expect(err).ToNot(HaveOccurred())
				})

				It("opens a new stream when receiving a WINDOW_UPDATE for an unknown stream", func() {
					err := session.handleWindowUpdateFrame(&frames.WindowUpdateFrame{
						StreamID:   5,
						ByteOffset: 1337,
					})
					Expect(err).ToNot(HaveOccurred())
					Expect(session.streams).To(HaveKey(protocol.StreamID(5)))
					Expect(session.streams[5]).ToNot(BeNil())
				})

				It("errors when receiving a WindowUpdateFrame for a closed stream", func() {
					session.streams[5] = nil // this is what the garbageCollectStreams() does when a Stream is closed
					err := session.handleWindowUpdateFrame(&frames.WindowUpdateFrame{
						StreamID:   5,
						ByteOffset: 1337,
					})
					Expect(err).To(MatchError(errWindowUpdateOnClosedStream))
				})

				It("ignores errors when receiving a WindowUpdateFrame for a closed stream", func() {
					session.streams[5] = nil // this is what the garbageCollectStreams() does when a Stream is closed
					err := session.handleFrames([]frames.Frame{&frames.WindowUpdateFrame{
						StreamID:   5,
						ByteOffset: 1337,
					}})
					Expect(err).NotTo(HaveOccurred())
				})
			})

			It("handles PING frames", func() {
				err := session.handleFrames([]frames.Frame{&frames.PingFrame{}})
				Expect(err).NotTo(HaveOccurred())
			})

			It("handles BLOCKED frames", func() {
				err := session.handleFrames([]frames.Frame{&frames.BlockedFrame{}})
				Expect(err).NotTo(HaveOccurred())
			})

			It("errors on GOAWAY frames", func() {
				err := session.handleFrames([]frames.Frame{&frames.GoawayFrame{}})
				Expect(err).To(MatchError("unimplemented: handling GOAWAY frames"))
			})

			It("handles STOP_WAITING frames", func() {
				err := session.handleFrames([]frames.Frame{&frames.StopWaitingFrame{LeastUnacked: 10}})
				Expect(err).NotTo(HaveOccurred())
			})

			It("handles CONNECTION_CLOSE frames", func() {
				str, _ := session.OpenStream(5)
				err := session.handleFrames([]frames.Frame{&frames.ConnectionCloseFrame{ErrorCode: 42, ReasonPhrase: "foobar"}})
				Expect(err).NotTo(HaveOccurred())
				_, err = str.Read([]byte{0})
				Expect(err).To(MatchError(qerr.Error(42, "foobar")))
			})

			Context("closing", func() {
				var (
					nGoRoutinesBefore int
				)

				BeforeEach(func() {
					time.Sleep(10 * time.Millisecond) // Wait for old goroutines to finish
					nGoRoutinesBefore = runtime.NumGoroutine()
					go session.run()
					Eventually(func() int { return runtime.NumGoroutine() }).Should(Equal(nGoRoutinesBefore + 2))
				})

				It("shuts down without error", func() {
					session.Close(nil)
					Expect(closeCallbackCalled).To(BeTrue())
					Eventually(func() int { return runtime.NumGoroutine() }).Should(Equal(nGoRoutinesBefore))
					Expect(conn.written).To(HaveLen(1))
					Expect(conn.written[0][len(conn.written[0])-7:]).To(Equal([]byte{0x02, byte(qerr.PeerGoingAway), 0, 0, 0, 0, 0}))
				})

				It("only closes once", func() {
					session.Close(nil)
					session.Close(nil)
					Eventually(func() int { return runtime.NumGoroutine() }).Should(Equal(nGoRoutinesBefore))
					Expect(conn.written).To(HaveLen(1))
				})

				It("closes streams with proper error", func() {
					testErr := errors.New("test error")
					s, err := session.OpenStream(5)
					Expect(err).NotTo(HaveOccurred())
					session.Close(testErr)
					Expect(closeCallbackCalled).To(BeTrue())
					Eventually(func() int { return runtime.NumGoroutine() }).Should(Equal(nGoRoutinesBefore))
					n, err := s.Read([]byte{0})
					Expect(n).To(BeZero())
					Expect(err.Error()).To(ContainSubstring(testErr.Error()))
					n, err = s.Write([]byte{0})
					Expect(n).To(BeZero())
					Expect(err.Error()).To(ContainSubstring(testErr.Error()))
				})
			})

			Context("receiving packets", func() {
				var hdr *publicHeader

				BeforeEach(func() {
					session.unpacker = &mockUnpacker{}
					hdr = &publicHeader{PacketNumberLen: protocol.PacketNumberLen6}
				})

				It("sets the lastRcvdPacketNumber", func() {
					hdr.PacketNumber = 5
					err := session.handlePacketImpl(nil, hdr, nil)
					Expect(err).ToNot(HaveOccurred())
					Expect(session.lastRcvdPacketNumber).To(Equal(protocol.PacketNumber(5)))
				})

				It("sets the lastRcvdPacketNumber, for an out-of-order packet", func() {
					hdr.PacketNumber = 5
					err := session.handlePacketImpl(nil, hdr, nil)
					Expect(err).ToNot(HaveOccurred())
					Expect(session.lastRcvdPacketNumber).To(Equal(protocol.PacketNumber(5)))
					hdr.PacketNumber = 3
					err = session.handlePacketImpl(nil, hdr, nil)
					Expect(err).ToNot(HaveOccurred())
					Expect(session.lastRcvdPacketNumber).To(Equal(protocol.PacketNumber(3)))
				})

				It("ignores duplicate packets", func() {
					hdr.PacketNumber = 5
					err := session.handlePacketImpl(nil, hdr, nil)
					Expect(err).ToNot(HaveOccurred())
					err = session.handlePacketImpl(nil, hdr, nil)
					Expect(err).ToNot(HaveOccurred())
				})

				It("ignores packets smaller than the highest LeastUnacked of a StopWaiting", func() {
					err := session.receivedPacketHandler.ReceivedStopWaiting(&frames.StopWaitingFrame{LeastUnacked: 10})
					Expect(err).ToNot(HaveOccurred())
					hdr.PacketNumber = 5
					err = session.handlePacketImpl(nil, hdr, nil)
					Expect(err).ToNot(HaveOccurred())
				})
			})

			Context("sending packets", func() {
				It("sends ack frames", func() {
					packetNumber := protocol.PacketNumber(0x35EA)
					session.receivedPacketHandler.ReceivedPacket(packetNumber, true)
					err := session.sendPacket()
					Expect(err).NotTo(HaveOccurred())
					Expect(conn.written).To(HaveLen(1))
					// test for the beginning of an ACK frame: Entropy until LargestObserved
					Expect(conn.written[0]).To(ContainSubstring(string([]byte{0xEA, 0x35})))
				})

				It("sends two WindowUpdate frames", func() {
					_, err := session.OpenStream(5)
					Expect(err).ToNot(HaveOccurred())
					session.flowControlManager.AddBytesRead(5, protocol.ReceiveStreamFlowControlWindow)
					err = session.sendPacket()
					Expect(err).NotTo(HaveOccurred())
					err = session.sendPacket()
					Expect(err).NotTo(HaveOccurred())
					err = session.sendPacket()
					Expect(err).NotTo(HaveOccurred())
					Expect(conn.written).To(HaveLen(2))
					Expect(conn.written[0]).To(ContainSubstring(string([]byte{0x04, 0x05, 0, 0, 0})))
					Expect(conn.written[1]).To(ContainSubstring(string([]byte{0x04, 0x05, 0, 0, 0})))
				})

				It("sends public reset", func() {
					err := session.sendPublicReset(1)
					Expect(err).NotTo(HaveOccurred())
					Expect(conn.written).To(HaveLen(1))
					Expect(conn.written[0]).To(ContainSubstring(string([]byte("PRST"))))
				})
			})

			Context("retransmissions", func() {
				It("sends a StreamFrame from a packet queued for retransmission", func() {
					f := frames.StreamFrame{
						StreamID: 0x5,
						Data:     []byte("foobar1234567"),
					}
					p := ackhandlerlegacy.Packet{
						PacketNumber: 0x1337,
						Frames:       []frames.Frame{&f},
					}
					sph := newMockSentPacketHandler()
					sph.(*mockSentPacketHandler).retransmissionQueue = []*ackhandlerlegacy.Packet{&p}
					session.sentPacketHandler = sph

					err := session.sendPacket()
					Expect(err).NotTo(HaveOccurred())
					Expect(conn.written).To(HaveLen(1))
					Expect(conn.written[0]).To(ContainSubstring("foobar1234567"))
				})

				It("sends a StreamFrame from a packet queued for retransmission", func() {
					f1 := frames.StreamFrame{
						StreamID: 0x5,
						Data:     []byte("foobar"),
					}
					f2 := frames.StreamFrame{
						StreamID: 0x7,
						Data:     []byte("loremipsum"),
					}
					p1 := ackhandlerlegacy.Packet{
						PacketNumber: 0x1337,
						Frames:       []frames.Frame{&f1},
					}
					p2 := ackhandlerlegacy.Packet{
						PacketNumber: 0x1338,
						Frames:       []frames.Frame{&f2},
					}
					sph := newMockSentPacketHandler()
					sph.(*mockSentPacketHandler).retransmissionQueue = []*ackhandlerlegacy.Packet{&p1, &p2}
					session.sentPacketHandler = sph

					err := session.sendPacket()
					Expect(err).NotTo(HaveOccurred())
					Expect(conn.written).To(HaveLen(1))
					Expect(conn.written[0]).To(ContainSubstring("foobar"))
					Expect(conn.written[0]).To(ContainSubstring("loremipsum"))
				})
			})

			Context("scheduling sending", func() {
				It("sends after writing to a stream", func(done Done) {
					Expect(session.sendingScheduled).NotTo(Receive())
					s, err := session.OpenStream(3)
					Expect(err).NotTo(HaveOccurred())
					go func() {
						s.Write([]byte("foobar"))
						close(done)
					}()
					Eventually(session.sendingScheduled).Should(Receive())
					s.(*stream).getDataForWriting(1000) // unblock
				})

				Context("bundling of small packets", func() {
					It("bundles two small frames of different streams into one packet", func() {
						s1, err := session.OpenStream(5)
						Expect(err).NotTo(HaveOccurred())
						s2, err := session.OpenStream(7)
						Expect(err).NotTo(HaveOccurred())
						go func() {
							time.Sleep(time.Millisecond)
							session.run()
						}()
						go func() {
							_, err2 := s1.Write([]byte("foobar1"))
							Expect(err2).NotTo(HaveOccurred())
						}()
						_, err = s2.Write([]byte("foobar2"))
						Expect(err).NotTo(HaveOccurred())
						time.Sleep(10 * time.Millisecond)
						Expect(conn.written).To(HaveLen(1))
					})

					It("sends out two big frames in two packets", func() {
						s1, err := session.OpenStream(5)
						Expect(err).NotTo(HaveOccurred())
						s2, err := session.OpenStream(7)
						Expect(err).NotTo(HaveOccurred())
						go session.run()
						go func() {
							defer GinkgoRecover()
							_, err2 := s1.Write(bytes.Repeat([]byte{'e'}, 1000))
							Expect(err2).ToNot(HaveOccurred())
						}()
						_, err = s2.Write(bytes.Repeat([]byte{'e'}, 1000))
						Expect(err).ToNot(HaveOccurred())
						Eventually(func() [][]byte { return conn.written }).Should(HaveLen(2))
					})

					It("sends out two small frames that are written to long after one another into two packets", func() {
						s, err := session.OpenStream(5)
						Expect(err).NotTo(HaveOccurred())
						go session.run()
						_, err = s.Write([]byte("foobar1"))
						Expect(err).NotTo(HaveOccurred())
						Eventually(func() [][]byte { return conn.written }).Should(HaveLen(1))
						_, err = s.Write([]byte("foobar2"))
						Expect(err).NotTo(HaveOccurred())
						Eventually(func() [][]byte { return conn.written }).Should(HaveLen(2))
					})

					It("sends a queued ACK frame only once", func() {
						packetNumber := protocol.PacketNumber(0x1337)
						session.receivedPacketHandler.ReceivedPacket(packetNumber, true)

						s, err := session.OpenStream(5)
						Expect(err).NotTo(HaveOccurred())
						go session.run()
						_, err = s.Write([]byte("foobar1"))
						Expect(err).NotTo(HaveOccurred())
						Eventually(func() [][]byte { return conn.written }).Should(HaveLen(1))
						_, err = s.Write([]byte("foobar2"))
						Expect(err).NotTo(HaveOccurred())

						Eventually(func() [][]byte { return conn.written }).Should(HaveLen(2))
						Expect(conn.written[0]).To(ContainSubstring(string([]byte{0x37, 0x13})))
						Expect(conn.written[1]).ToNot(ContainSubstring(string([]byte{0x37, 0x13})))
					})
				})
			})

			It("closes when crypto stream errors", func() {
				go session.run()
				s, err := session.OpenStream(3)
				Expect(err).NotTo(HaveOccurred())
				err = session.handleStreamFrame(&frames.StreamFrame{
					StreamID: 1,
					Data:     []byte("4242\x00\x00\x00\x00"),
				})
				Expect(err).NotTo(HaveOccurred())
				Eventually(func() bool { return atomic.LoadUint32(&session.closed) != 0 }).Should(BeTrue())
				_, err = s.Write([]byte{})
				Expect(err.(*qerr.QuicError).ErrorCode).To(Equal(qerr.InvalidCryptoMessageType))
			})

			It("sends public reset after too many undecryptable packets", func() {
				// Write protocol.MaxUndecryptablePackets and expect a public reset to happen
				for i := 0; i < protocol.MaxUndecryptablePackets; i++ {
					hdr := &publicHeader{
						PacketNumber: protocol.PacketNumber(i + 1),
					}
					session.handlePacket(nil, hdr, []byte("foobar"))
				}
				session.run()

				Expect(conn.written).To(HaveLen(1))
				Expect(conn.written[0]).To(ContainSubstring(string([]byte("PRST"))))
			})

			It("unqueues undecryptable packets for later decryption", func() {
				session.undecryptablePackets = []receivedPacket{{
					nil,
					&publicHeader{PacketNumber: protocol.PacketNumber(42)},
					nil,
				}}
				Expect(session.receivedPackets).NotTo(Receive())
				session.tryDecryptingQueuedPackets()
				Expect(session.undecryptablePackets).To(BeEmpty())
				Expect(session.receivedPackets).To(Receive())
			})

			It("times out", func(done Done) {
				session.connectionParametersManager.SetFromMap(map[handshake.Tag][]byte{
					handshake.TagICSL: {0, 0, 0, 0},
				})
				session.packer.connectionParametersManager = session.connectionParametersManager
				session.run() // Would normally not return
				Expect(conn.written[0]).To(ContainSubstring("No recent network activity."))
				close(done)
			}, 3)

			It("errors when the SentPacketHandler has too many packets tracked", func() {
				streamFrame := frames.StreamFrame{StreamID: 5, Data: []byte("foobar")}
				for i := uint32(1); i < protocol.MaxTrackedSentPackets+10; i++ {
					packet := ackhandlerlegacy.Packet{PacketNumber: protocol.PacketNumber(i), Frames: []frames.Frame{&streamFrame}, Length: 1}
					err := session.sentPacketHandler.SentPacket(&packet)
					Expect(err).ToNot(HaveOccurred())
				}
				// now session.sentPacketHandler.CheckForError will return an error
				err := session.sendPacket()
				Expect(err).To(MatchError(ackhandlerlegacy.ErrTooManyTrackedSentPackets))
			})

			It("stores up to MaxSessionUnprocessedPackets packets", func(done Done) {
				// Nothing here should block
				for i := 0; i < protocol.MaxSessionUnprocessedPackets+10; i++ {
					session.handlePacket(nil, nil, nil)
				}
				close(done)
			}, 0.5)

			It("retransmits RTO packets", func() {
				// We simulate consistently low RTTs, so that the test works faster
				n := protocol.PacketNumber(10)
				for p := protocol.PacketNumber(1); p < n; p++ {
					err := session.sentPacketHandler.SentPacket(&ackhandlerlegacy.Packet{PacketNumber: p, Length: 1})
					Expect(err).NotTo(HaveOccurred())
					time.Sleep(time.Microsecond)
					ack := frames.AckFrameLegacy{LargestObserved: p}
					err = session.sentPacketHandler.ReceivedAck(&frames.AckFrame{AckFrameLegacy: &ack}, p)
					Expect(err).NotTo(HaveOccurred())
				}
				// Now, we send a single packet, and expect that it was retransmitted later
				go session.run()
				Expect(conn.written).To(BeEmpty())
				err := session.sentPacketHandler.SentPacket(&ackhandlerlegacy.Packet{
					PacketNumber: n,
					Length:       1,
					Frames: []frames.Frame{&frames.StreamFrame{
						Data: bytes.Repeat([]byte{'a'}, 1000),
					}},
				})
				session.packer.lastPacketNumber = n
				Expect(err).NotTo(HaveOccurred())
				session.scheduleSending()
				Eventually(func() bool { return len(conn.written) > 0 }).Should(BeTrue())
			})

			Context("counting streams", func() {
				It("errors when too many streams are opened", func(done Done) {
					// 1.1 * 100
					for i := 2; i <= 110; i++ {
						_, err := session.OpenStream(protocol.StreamID(i))
						Expect(err).NotTo(HaveOccurred())
					}
					_, err := session.OpenStream(protocol.StreamID(111))
					Expect(err).To(MatchError(qerr.TooManyOpenStreams))
					Eventually(session.closeChan).Should(Receive())
					close(done)
				})

				It("does not error when many streams are opened and closed", func() {
					for i := 2; i <= 1000; i++ {
						s, err := session.OpenStream(protocol.StreamID(i))
						Expect(err).NotTo(HaveOccurred())
						err = s.Close()
						Expect(err).NotTo(HaveOccurred())
						s.(*stream).sentFin()
						s.CloseRemote(0)
						_, err = s.Read([]byte("a"))
						Expect(err).To(MatchError(io.EOF))
						session.garbageCollectStreams()
					}
				})
			})

			Context("ignoring errors", func() {
				It("ignores duplicate acks", func() {
					session.sentPacketHandler.SentPacket(&ackhandlerlegacy.Packet{
						PacketNumber: 1,
						Length:       1,
					})
					err := session.handleFrames([]frames.Frame{&frames.AckFrame{
						AckFrameLegacy: &frames.AckFrameLegacy{
							LargestObserved: 1,
						},
					}})
					Expect(err).NotTo(HaveOccurred())
					err = session.handleFrames([]frames.Frame{&frames.AckFrame{
						AckFrameLegacy: &frames.AckFrameLegacy{
							LargestObserved: 1,
						},
					}})
					Expect(err).NotTo(HaveOccurred())
				})
			})

			Context("window updates", func() {
				It("gets stream level window updates", func() {
					err := session.flowControlManager.AddBytesRead(1, protocol.ReceiveStreamFlowControlWindow)
					Expect(err).NotTo(HaveOccurred())
					frames, err := session.getWindowUpdateFrames()
					Expect(err).NotTo(HaveOccurred())
					Expect(frames).To(HaveLen(1))
					Expect(frames[0].StreamID).To(Equal(protocol.StreamID(1)))
					Expect(frames[0].ByteOffset).To(Equal(protocol.ReceiveStreamFlowControlWindow * 2))
				})

				It("gets connection level window updates", func() {
					_, err := session.OpenStream(5)
					Expect(err).NotTo(HaveOccurred())
					err = session.flowControlManager.AddBytesRead(5, protocol.ReceiveConnectionFlowControlWindow)
					Expect(err).NotTo(HaveOccurred())
					frames, err := session.getWindowUpdateFrames()
					Expect(err).NotTo(HaveOccurred())
					Expect(frames).To(HaveLen(1))
					Expect(frames[0].StreamID).To(Equal(protocol.StreamID(0)))
					Expect(frames[0].ByteOffset).To(Equal(protocol.ReceiveConnectionFlowControlWindow * 2))
				})
			})
		})
	}
})
