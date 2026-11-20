import Foundation
import Network
import Synchronization

/// Accepts TCP and reads the client's greeting but never answers it. Tests can stop a real SSH handshake at a
/// known point without a firewall, a remote server, or a race with a connection-refused error.
final class QuietSSHServer: @unchecked Sendable {
    private struct State {
        var connections: [NWConnection] = []
        var closed: Set<ObjectIdentifier> = []
    }
    private let state = Mutex(State())
    private let listener: NWListener
    private let queue = DispatchQueue(label: "persona.tests.quiet-ssh")

    init() async throws {
        let parameters = NWParameters.tcp
        parameters.requiredLocalEndpoint = .hostPort(host: "127.0.0.1", port: .any)
        listener = try NWListener(using: parameters)
        listener.newConnectionHandler = { [weak self] connection in
            guard let self else { connection.cancel(); return }
            self.state.withLock { $0.connections.append(connection) }
            connection.start(queue: self.queue)
            self.receive(connection)
        }
        try await withCheckedThrowingContinuation { (continuation: CheckedContinuation<Void, Error>) in
            let waiting = Mutex<CheckedContinuation<Void, Error>?>(continuation)
            listener.stateUpdateHandler = { status in
                let result: Result<Void, Error>
                switch status {
                case .ready: result = .success(())
                case .failed(let error): result = .failure(error)
                case .cancelled: result = .failure(CancellationError())
                default: return
                }
                let ready = waiting.withLock { value in defer { value = nil }; return value }
                ready?.resume(with: result)
            }
            listener.start(queue: queue)
        }
    }

    var port: Int { Int(listener.port!.rawValue) }
    var accepted: Int { state.withLock { $0.connections.count } }
    var closed: Int { state.withLock { $0.closed.count } }

    private func receive(_ connection: NWConnection) {
        connection.receive(minimumIncompleteLength: 1, maximumLength: 65_536) { [weak self] _, _, complete, error in
            guard let self else { return }
            if complete || error != nil {
                self.state.withLock { _ = $0.closed.insert(ObjectIdentifier(connection)) }
                connection.cancel()
            } else {
                self.receive(connection)
            }
        }
    }

    func stop() {
        listener.cancel()
        for connection in state.withLock({ $0.connections }) { connection.cancel() }
    }

    deinit { stop() }
}
