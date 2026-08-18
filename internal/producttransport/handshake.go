package producttransport

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"time"
)

const (
	agentPreface  = "DPPA"
	serverPreface = "DPPS"
)

func agentHandshake(ctx context.Context, conn net.Conn, credential []byte, incarnation uint64, maxCredential int) (SessionInfo, error) {
	return agentHandshakeVersion(ctx, conn, credential, incarnation, maxCredential, CurrentProductProtocolVersion)
}

func agentHandshakeVersion(ctx context.Context, conn net.Conn, credential []byte, incarnation uint64, maxCredential int, offeredVersion uint32) (SessionInfo, error) {
	if len(credential) == 0 || len(credential) > maxCredential || incarnation == 0 {
		return SessionInfo{}, fmt.Errorf("%w: invalid credential length or incarnation", ErrProtocol)
	}
	reset := applyDeadline(ctx, conn)
	defer reset()
	header := make([]byte, 20)
	copy(header, agentPreface)
	binary.BigEndian.PutUint32(header[4:8], offeredVersion)
	binary.BigEndian.PutUint64(header[8:16], incarnation)
	binary.BigEndian.PutUint32(header[16:20], uint32(len(credential)))
	if err := writeFull(conn, header); err != nil {
		return SessionInfo{}, err
	}
	if err := writeFull(conn, credential); err != nil {
		return SessionInfo{}, err
	}
	response := make([]byte, 30)
	if _, err := io.ReadFull(conn, response); err != nil {
		return SessionInfo{}, err
	}
	if string(response[:4]) != serverPreface {
		return SessionInfo{}, fmt.Errorf("%w: invalid server preface", ErrProtocol)
	}
	version := binary.BigEndian.Uint32(response[4:8])
	if !supportedProductProtocolVersion(version) || version != offeredVersion {
		return SessionInfo{}, fmt.Errorf("%w: unsupported negotiated version %d", ErrProtocol, version)
	}
	agentLength := int(binary.BigEndian.Uint16(response[24:26]))
	credentialIDLength := int(binary.BigEndian.Uint16(response[26:28]))
	serverIdentityLength := int(binary.BigEndian.Uint16(response[28:30]))
	if agentLength == 0 || agentLength > 1024 || credentialIDLength == 0 || credentialIDLength > 1024 || serverIdentityLength == 0 || serverIdentityLength > 1024 {
		return SessionInfo{}, fmt.Errorf("%w: invalid response identity lengths", ErrProtocol)
	}
	identities := make([]byte, agentLength+credentialIDLength+serverIdentityLength)
	if _, err := io.ReadFull(conn, identities); err != nil {
		return SessionInfo{}, err
	}
	return SessionInfo{
		SessionID:        SessionID(hex.EncodeToString(response[8:24])),
		AgentID:          string(identities[:agentLength]),
		CredentialID:     string(identities[agentLength : agentLength+credentialIDLength]),
		ServerIdentityID: string(identities[agentLength+credentialIDLength:]),
		Incarnation:      incarnation,
		ProtocolVersion:  version,
	}, nil
}

func serverHandshake(ctx context.Context, conn net.Conn, verifier CredentialVerifier, now time.Time, maxCredential int, random io.Reader) (SessionInfo, error) {
	reset := applyDeadline(ctx, conn)
	defer reset()
	header := make([]byte, 20)
	if _, err := io.ReadFull(conn, header); err != nil {
		return SessionInfo{}, err
	}
	if string(header[:4]) != agentPreface {
		return SessionInfo{}, fmt.Errorf("%w: invalid agent preface", ErrProtocol)
	}
	version := binary.BigEndian.Uint32(header[4:8])
	if !supportedProductProtocolVersion(version) {
		return SessionInfo{}, fmt.Errorf("%w: unsupported agent version %d", ErrProtocol, version)
	}
	incarnation := binary.BigEndian.Uint64(header[8:16])
	credentialLength := int(binary.BigEndian.Uint32(header[16:20]))
	if incarnation == 0 || credentialLength == 0 || credentialLength > maxCredential {
		return SessionInfo{}, fmt.Errorf("%w: invalid credential length or incarnation", ErrProtocol)
	}
	credential := make([]byte, credentialLength)
	if _, err := io.ReadFull(conn, credential); err != nil {
		return SessionInfo{}, err
	}
	identity, err := verifier.VerifyCredential(ctx, credential, now)
	if err != nil {
		return SessionInfo{}, fmt.Errorf("%w: %w", ErrAuthentication, err)
	}
	if identity.AgentID == "" || identity.CredentialID == "" || identity.ServerIdentityID == "" ||
		len(identity.AgentID) > 1024 || len(identity.CredentialID) > 1024 || len(identity.ServerIdentityID) > 1024 {
		return SessionInfo{}, fmt.Errorf("%w: verifier returned an incomplete identity", ErrAuthentication)
	}
	sessionBytes := make([]byte, 16)
	if _, err := io.ReadFull(random, sessionBytes); err != nil {
		return SessionInfo{}, fmt.Errorf("generate session ID: %w", err)
	}
	response := make([]byte, 30+len(identity.AgentID)+len(identity.CredentialID)+len(identity.ServerIdentityID))
	copy(response, serverPreface)
	binary.BigEndian.PutUint32(response[4:8], version)
	copy(response[8:24], sessionBytes)
	binary.BigEndian.PutUint16(response[24:26], uint16(len(identity.AgentID)))
	binary.BigEndian.PutUint16(response[26:28], uint16(len(identity.CredentialID)))
	binary.BigEndian.PutUint16(response[28:30], uint16(len(identity.ServerIdentityID)))
	copy(response[30:], identity.AgentID)
	copy(response[30+len(identity.AgentID):], identity.CredentialID)
	copy(response[30+len(identity.AgentID)+len(identity.CredentialID):], identity.ServerIdentityID)
	if err := writeFull(conn, response); err != nil {
		return SessionInfo{}, err
	}
	return SessionInfo{
		SessionID:        SessionID(hex.EncodeToString(sessionBytes)),
		AgentID:          identity.AgentID,
		CredentialID:     identity.CredentialID,
		ServerIdentityID: identity.ServerIdentityID,
		Incarnation:      incarnation,
		ProtocolVersion:  version,
	}, nil
}

func writeFull(writer io.Writer, payload []byte) error {
	for len(payload) > 0 {
		written, err := writer.Write(payload)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		payload = payload[written:]
	}
	return nil
}

func applyDeadline(ctx context.Context, conn net.Conn) func() {
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
		return func() { _ = conn.SetDeadline(time.Time{}) }
	}
	return func() {}
}
