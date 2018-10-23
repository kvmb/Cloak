package multiplex

import (
	"errors"
	"log"
	"net"
	"sync"
	"sync/atomic"
)

const (
	errBrokenSession        = "broken session"
	errRepeatSessionClosing = "trying to close a closed session"
	// Copied from smux
	acceptBacklog = 1024

	closeBacklog = 512
)

type Session struct {
	id int

	// Used in Stream.Write. Add multiplexing headers, encrypt and add TLS header
	obfs func(*Frame) []byte
	// Remove TLS header, decrypt and unmarshall multiplexing headers
	deobfs func([]byte) *Frame
	// This is supposed to read one TLS message, the same as GoQuiet's ReadTillDrain
	obfsedReader func(net.Conn, []byte) (int, error)

	nextStreamID uint32

	streamsM sync.RWMutex
	streams  map[uint32]*Stream

	// Switchboard manages all connections to remote
	sb *switchboard

	// For accepting new streams
	acceptCh chan *Stream
	// Once a stream.Close is called, it sends its streamID to this channel
	// to be read by another stream to send the streamID to notify the remote
	// that this stream is closed
	closeQCh chan uint32

	closingM sync.Mutex
	die      chan struct{}
	closing  bool
}

// 1 conn is needed to make a session
func MakeSession(id int, conn net.Conn, obfs func(*Frame) []byte, deobfs func([]byte) *Frame, obfsedReader func(net.Conn, []byte) (int, error)) *Session {
	sesh := &Session{
		id:           id,
		obfs:         obfs,
		deobfs:       deobfs,
		obfsedReader: obfsedReader,
		nextStreamID: 1,
		streams:      make(map[uint32]*Stream),
		acceptCh:     make(chan *Stream, acceptBacklog),
		closeQCh:     make(chan uint32, closeBacklog),
	}
	sesh.sb = makeSwitchboard(conn, sesh)
	return sesh
}

func (sesh *Session) AddConnection(conn net.Conn) {
	sesh.sb.newConnCh <- conn
}

func (sesh *Session) OpenStream() (*Stream, error) {
	id := atomic.AddUint32(&sesh.nextStreamID, 1)
	id -= 1 // Because atomic.AddUint32 returns the value after incrementation
	stream := makeStream(id, sesh)
	sesh.streamsM.Lock()
	sesh.streams[id] = stream
	sesh.streamsM.Unlock()
	return stream, nil
}

func (sesh *Session) AcceptStream() (*Stream, error) {
	select {
	case <-sesh.die:
		return nil, errors.New(errBrokenSession)
	case stream := <-sesh.acceptCh:
		return stream, nil
	}

}

func (sesh *Session) delStream(id uint32) {
	sesh.streamsM.Lock()
	delete(sesh.streams, id)
	sesh.streamsM.Unlock()
}

func (sesh *Session) isStream(id uint32) bool {
	sesh.streamsM.RLock()
	_, ok := sesh.streams[id]
	sesh.streamsM.RUnlock()
	return ok
}

func (sesh *Session) getStream(id uint32) *Stream {
	sesh.streamsM.RLock()
	defer sesh.streamsM.RUnlock()
	return sesh.streams[id]
}

// addStream is used when the remote opened a new stream and we got notified
func (sesh *Session) addStream(id uint32) *Stream {
	log.Printf("Adding stream %v", id)
	stream := makeStream(id, sesh)
	sesh.streamsM.Lock()
	sesh.streams[id] = stream
	sesh.streamsM.Unlock()
	sesh.acceptCh <- stream
	return stream
}

func (sesh *Session) Close() error {
	// Because closing a closed channel causes panic
	sesh.closingM.Lock()
	defer sesh.closingM.Unlock()
	if sesh.closing {
		return errors.New(errRepeatSessionClosing)
	}
	sesh.closing = true
	close(sesh.die)
	sesh.streamsM.Lock()
	for id, stream := range sesh.streams {
		// If we call stream.Close() here, streamsM will result in a deadlock
		// because stream.Close calls sesh.delStream, which locks the mutex.
		// so we need to implement a method of stream that closes the stream without calling
		// sesh.delStream
		// This can also be seen in smux
		go stream.closeNoDelMap()
		delete(sesh.streams, id)
	}
	sesh.streamsM.Unlock()

	return nil

}