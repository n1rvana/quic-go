package handshake

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"sort"

	"github.com/lucas-clemente/quic-go/protocol"
	"github.com/lucas-clemente/quic-go/qerr"
	"github.com/lucas-clemente/quic-go/utils"
)

// ParseHandshakeMessage reads a crypto message
func ParseHandshakeMessage(r utils.ReadStream) (Tag, map[Tag][]byte, error) {
	messageTag, err := utils.ReadUint32(r)
	if err != nil {
		return 0, nil, err
	}

	nPairs, err := utils.ReadUint32(r)
	if err != nil {
		return 0, nil, err
	}

	if nPairs > protocol.CryptoMaxParams {
		return 0, nil, qerr.CryptoTooManyEntries
	}

	index := make([]byte, nPairs*8)
	_, err = io.ReadFull(r, index)
	if err != nil {
		return 0, nil, err
	}

	resultMap := map[Tag][]byte{}

	var dataStart uint32
	for indexPos := 0; indexPos < int(nPairs)*8; indexPos += 8 {
		tag := Tag(binary.LittleEndian.Uint32(index[indexPos : indexPos+4]))
		dataEnd := uint32(binary.LittleEndian.Uint32(index[indexPos+4 : indexPos+8]))

		dataLen := dataEnd - dataStart
		if dataLen > protocol.CryptoParameterMaxLength {
			return 0, nil, qerr.Error(qerr.CryptoInvalidValueLength, "value too long")
		}

		data := make([]byte, dataLen)
		_, err = io.ReadFull(r, data)
		if err != nil {
			return 0, nil, err
		}

		resultMap[tag] = data
		dataStart = dataEnd
	}

	return Tag(messageTag), resultMap, nil
}

// WriteHandshakeMessage writes a crypto message
func WriteHandshakeMessage(b *bytes.Buffer, messageTag Tag, data map[Tag][]byte) {
	utils.WriteUint32(b, uint32(messageTag))
	utils.WriteUint16(b, uint16(len(data)))
	utils.WriteUint16(b, 0)

	// Save current position in the buffer, so that we can update the index in-place later
	indexStart := b.Len()

	indexData := make([]byte, 8*len(data))
	b.Write(indexData) // Will be updated later

	// Sort the tags
	tags := make([]uint32, len(data))
	i := 0
	for t := range data {
		tags[i] = uint32(t)
		i++
	}
	sort.Sort(utils.Uint32Slice(tags))

	offset := uint32(0)
	for i, t := range tags {
		v := data[Tag(t)]
		b.Write(v)
		offset += uint32(len(v))
		binary.LittleEndian.PutUint32(indexData[i*8:], t)
		binary.LittleEndian.PutUint32(indexData[i*8+4:], offset)
	}

	// Now we write the index data for real
	copy(b.Bytes()[indexStart:], indexData)
}

func printHandshakeMessage(data map[Tag][]byte) string {
	var res string
	for k, v := range data {
		if k == TagPAD {
			continue
		}
		res += fmt.Sprintf("\t%s: %#v\n", tagToString(k), string(v))
	}
	return res
}

func tagToString(tag Tag) string {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, uint32(tag))
	for i := range b {
		if b[i] == 0 {
			b[i] = ' '
		}
	}
	return string(b)
}
