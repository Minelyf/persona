import Foundation
import Synchronization
import Testing
@testable import PersonaSSH

@Suite(.timeLimit(.minutes(1)))
struct SSHCancelTests {
    private func tunnel(_ server: QuietSSHServer) -> SSHTunnel {
        // A stalled handshake normally waits 25 s. These tests require cancellation within 2 s instead.
        SSHTunnel(config: SSHTunnelConfig(host: "127.0.0.1", port: server.port, user: "test"),
                  identity: .generate(), pinnedHostKey: nil)
    }

    @Test func stopEndsAnUnfinishedHandshakeAndClosesItsTCPConnection() async throws {
        let server = try await QuietSSHServer()
        defer { server.stop() }
        let tunnel = tunnel(server)
        defer { tunnel.stop() }
        let result = Mutex<Result<Int, Error>?>(nil)
        let start = Task {
            do {
                let port = try await tunnel.start()
                result.withLock { $0 = .success(port) }
            }
            catch { result.withLock { $0 = .failure(error) } }
        }
        try await until { server.accepted == 1 }
        tunnel.stop()
        try await until { result.withLock { $0 != nil } && server.closed == 1 }
        await start.value
        if case .failure(let error)? = result.withLock({ $0 }) {
            #expect(error as? SSHTunnelError == .stopped)
        } else {
            Issue.record("a stopped handshake unexpectedly succeeded")
        }
        #expect(tunnel.phase == .idle)
        #expect(tunnel.localPort == 0)
    }

    @Test func cancellingStartStopsItsHandshake() async throws {
        let server = try await QuietSSHServer()
        defer { server.stop() }
        let tunnel = tunnel(server)
        defer { tunnel.stop() }
        let finished = Mutex(false)
        let start = Task {
            defer { finished.withLock { $0 = true } }
            do { _ = try await tunnel.start(); Issue.record("cancelled start succeeded") }
            catch { #expect(error is CancellationError || error as? SSHTunnelError == .stopped) }
        }
        try await until { server.accepted == 1 }
        start.cancel()
        try await until { finished.withLock { $0 } && server.closed == 1 }
        await start.value
        #expect(tunnel.phase == .idle)
    }

    @Test func cancellingVerificationStopsItsHandshake() async throws {
        let server = try await QuietSSHServer()
        defer { server.stop() }
        let tunnel = tunnel(server)
        defer { tunnel.stop() }
        let finished = Mutex(false)
        let verify = Task {
            defer { finished.withLock { $0 = true } }
            do { _ = try await tunnel.verify(); Issue.record("cancelled verification succeeded") }
            catch { #expect(error is CancellationError || error as? SSHTunnelError == .stopped) }
        }
        try await until { server.accepted == 1 }
        verify.cancel()
        try await until { finished.withLock { $0 } && server.closed == 1 }
        await verify.value
        #expect(tunnel.phase == .idle)
    }

    private func until(_ condition: () -> Bool) async throws {
        let deadline = ContinuousClock.now + .seconds(2)
        while !condition() {
            try #require(ContinuousClock.now < deadline, "cancellation waited for the handshake timeout")
            try await Task.sleep(for: .milliseconds(10))
        }
    }
}
