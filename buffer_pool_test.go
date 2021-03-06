package quic

import (
	"github.com/lucas-clemente/quic-go/protocol"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("Buffer Pool", func() {
	It("returns buffers of correct len and cap", func() {
		buf := getPacketBuffer()
		Expect(buf).To(HaveLen(0))
		Expect(buf).To(HaveCap(int(protocol.MaxPacketSize)))
	})

	It("zeroes put buffers' length", func() {
		for i := 0; i < 1000; i++ {
			buf := getPacketBuffer()
			putPacketBuffer(buf[0:10])
			buf = getPacketBuffer()
			Expect(buf).To(HaveLen(0))
			Expect(buf).To(HaveCap(int(protocol.MaxPacketSize)))
		}
	})
})
