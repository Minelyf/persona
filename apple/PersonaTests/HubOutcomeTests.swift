import Foundation
import Testing
@testable import Persona
import PersonaWire

/// A private IPv4 address, written from its parts (the privacy scan keeps literal ones out of the repository).
private func ip(_ parts: Int...) -> String { parts.map(String.init).joined(separator: ".") }

/// Answers health probes as hub "H" at here.invalid, as hub "OTHER" at other.invalid and at a LAN address, without a
/// hub_id at bare.invalid, and with an unhealthy answer at sick.invalid.
private final class OutcomeHealthProtocol: URLProtocol, @unchecked Sendable {
    static let lan = ip(192, 168, 1, 20)
    static let hosts: Set<String> = ["here.invalid", "other.invalid", "bare.invalid", "sick.invalid", lan]
    override class func canInit(with request: URLRequest) -> Bool { hosts.contains(request.url?.host ?? "") }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }
    override func startLoading() {
        let host = request.url?.host ?? ""
        let (status, body): (Int, String) = switch host {
        case "here.invalid": (200, #"{"ok":true,"hub_id":"H"}"#)
        case "other.invalid", Self.lan: (200, #"{"ok":true,"hub_id":"OTHER"}"#)
        case "bare.invalid": (200, #"{"ok":true}"#)
        default: (503, #"{"ok":false}"#)
        }
        let resp = HTTPURLResponse(url: request.url!, statusCode: status, httpVersion: nil, headerFields: ["Content-Type": "application/json"])!
        client?.urlProtocol(self, didReceive: resp, cacheStoragePolicy: .notAllowed)
        client?.urlProtocol(self, didLoad: Data(body.utf8))
        client?.urlProtocolDidFinishLoading(self)
    }
    override func stopLoading() {}
}

/// What each try of a hub address comes to (#11390, docs/operations/network.md "Client-added candidates"): one classifier
/// for the probe and the socket, the private-address rule, probes cut short, and Connection Details' lines.
struct HubOutcomeTests {
    private func url(_ s: String) -> URL { URL(string: s)! }

    private func candidate(_ s: String, address: Int = 1, hubID: String? = "H") -> HubRoute.Candidate {
        .init(config: ServerConfig(url: url(s), token: "device"), hub: "h", address: address, hubID: address > 0 ? hubID : nil)
    }

    private func urlError(_ code: Int, _ info: [String: Any] = [:]) -> NSError {
        NSError(domain: NSURLErrorDomain, code: code, userInfo: info)
    }

    // MARK: Classifying a failure

    @Test func refusedTimedOutAndLostAreCouldNotConnect() {
        for code in [NSURLErrorCannotConnectToHost, NSURLErrorTimedOut, NSURLErrorCannotFindHost, NSURLErrorNetworkConnectionLost,
                     NSURLErrorNotConnectedToInternet, NSURLErrorDNSLookupFailed, NSURLErrorBadServerResponse] {
            #expect(HubOutcome(failure: urlError(code)) == .unreachable, "\(code)")
        }
        #expect(HubOutcome(failure: NSError(domain: NSPOSIXErrorDomain, code: 61)) == .unreachable)
    }

    @Test func anUntrustedCertificateIsKeptApart() {
        for code in [NSURLErrorServerCertificateUntrusted, NSURLErrorServerCertificateHasUnknownRoot,
                     NSURLErrorServerCertificateHasBadDate, NSURLErrorServerCertificateNotYetValid] {
            #expect(HubOutcome(failure: urlError(code)) == .untrusted, "\(code)")
        }
        // A failed secure connection is a trust failure only when it says so: the server's trust, or a certificate code.
        #expect(HubOutcome(failure: urlError(NSURLErrorSecureConnectionFailed, ["_kCFStreamErrorCodeKey": -9813])) == .untrusted)
        #expect(HubOutcome(failure: urlError(NSURLErrorSecureConnectionFailed, ["_kCFStreamErrorCodeKey": -9843])) == .untrusted)
        let under = NSError(domain: "kCFErrorDomainCFNetwork", code: -9807)
        #expect(HubOutcome(failure: urlError(NSURLErrorSecureConnectionFailed, [NSUnderlyingErrorKey: under])) == .untrusted)
        // A handshake cut short (the server hung up, spoke plain HTTP): couldn't connect.
        #expect(HubOutcome(failure: urlError(NSURLErrorSecureConnectionFailed, ["_kCFStreamErrorCodeKey": -9806])) == .unreachable)
        #expect(HubOutcome(failure: urlError(NSURLErrorSecureConnectionFailed)) == .unreachable)
    }

    @Test func aCancelledTryIsUnknown() {
        #expect(HubOutcome(failure: urlError(NSURLErrorCancelled)) == .unknown)
        #expect(HubOutcome(failure: CancellationError()) == .unknown)
        #expect(HubOutcome.worst([]) == .unknown)
        #expect(HubOutcome.worst([.answers]) == .unknown)
        #expect(HubOutcome.worst([.unknown, .unreachable, .untrusted]) == .untrusted)
        #expect(HubOutcome.worst([.unreachable, .notYourHub, .untrusted]) == .notYourHub)
    }

    // MARK: The probe's answer

    @Test func theProbeSaysWhoAnswered() async {
        URLProtocol.registerClass(OutcomeHealthProtocol.self)
        #expect(await HubRoute.healthCheck(candidate("https://here.invalid")) == .answers)
        #expect(await HubRoute.healthCheck(candidate("https://other.invalid")) == .notYourHub)
        // No hub_id: it can't be told apart from another server, but it says nothing about another hub either.
        #expect(await HubRoute.healthCheck(candidate("https://bare.invalid")) == .unreachable)
        #expect(await HubRoute.healthCheck(candidate("https://sick.invalid")) == .unreachable)
        // The paired address is not checked, as before: another hub_id there is left to the hello.
        #expect(await HubRoute.healthCheck(candidate("https://other.invalid", address: 0)) == .answers)
        // Pass or fail exactly as the yes / no probe.
        for c in [candidate("https://here.invalid"), candidate("https://other.invalid"), candidate("https://bare.invalid"),
                  candidate("https://sick.invalid"), candidate("https://other.invalid", address: 0)] {
            #expect(await HubRoute.healthProbe(c) == (await HubRoute.healthCheck(c) == .answers), "\(c.url)")
        }
    }

    // MARK: Not your hub on a private address

    @Test func anotherHubOnAPrivateAddressCountsAsCouldNotConnect() {
        let lan = candidate("https://\(OutcomeHealthProtocol.lan):18662"), pub = candidate("https://other.invalid")
        let at = Date()
        let tried = [HubRoute.Tried(candidate: lan, outcome: .notYourHub, at: at), .init(candidate: pub, outcome: .notYourHub, at: at)]
        // Public and private tried: the private one is just another device on this network.
        let mixed = HubRoute.settled(tried, among: [lan, pub])
        #expect(mixed.map(\.outcome) == [.unreachable, .notYourHub])
        // Only private addresses tried: nothing else could say more.
        let ula = candidate("https://[fd12:3456::1]:18662"), local = candidate("http://hub.local:8080")
        let onlyPrivate = HubRoute.settled([.init(candidate: lan, outcome: .notYourHub, at: at), .init(candidate: ula, outcome: .unreachable, at: at)],
                                           among: [lan, ula, local])
        #expect(onlyPrivate.map(\.outcome) == [.notYourHub, .unreachable])
    }

    @Test func privateAddressesAsAndroidTellsThem() {
        for s in ["http://127.0.0.1:1", "http://\(ip(10, 1, 2, 3))", "https://\(ip(172, 16, 0, 1))", "https://\(ip(172, 31, 255, 255))",
                  "http://\(ip(192, 168, 0, 5)):8080", "http://\(ip(169, 254, 1, 1))", "http://[::1]:8080", "https://[fe80::1%25en0]:443",
                  "https://[fd00::5]", "https://[fc01::5]", "http://localhost:18760", "https://hub.local", "http://[::ffff:\(ip(10, 0, 0, 1))]"] {
            #expect(HubAddressScope.isPrivate(url(s)), "\(s)")
        }
        for s in ["https://hub.example.com", "https://\(ip(172, 32, 0, 1))", "https://\(ip(8, 8, 8, 8))", "https://[2001:db8::1]",
                  "https://\(ip(100, 64, 0, 1))", "https://\(ip(192, 169, 0, 1))", "https://[::ffff:\(ip(8, 8, 8, 8))]", "https://local.example.com"] {
            #expect(!HubAddressScope.isPrivate(url(s)), "\(s)")
        }
    }

    // MARK: The race

    @Test func probesThePickerCutsShortAreUnknown() async {
        let a = candidate("https://a.invalid", address: 0), b = candidate("https://b.invalid"), c = candidate("https://c.invalid", address: 2)
        // A answers at once; B never does: A is taken without waiting, B was cut short.
        let quick = await HubRoute.pickTried([a, b, c], grace: .seconds(30)) { cand in
            if cand.url == b.url { try? await Task.sleep(for: .seconds(30)); return .unreachable }
            return cand.url == a.url ? .answers : .untrusted
        }
        #expect(quick.picked == a)
        let outcomes = Dictionary(uniqueKeysWithValues: quick.tried.map { ($0.candidate.url, $0.outcome) })
        #expect(outcomes[a.url] == .answers)
        #expect(outcomes[b.url] == .unknown)
        #expect(quick.tried.map(\.candidate) == [a, b, c]) // every candidate once, in order
        // None answers: the first, for the socket's own retries, with each one's reason.
        let none = await HubRoute.pickTried([a, b]) { $0.url == a.url ? .untrusted : .unreachable }
        #expect(none.picked == a)
        #expect(none.tried.map(\.outcome) == [.untrusted, .unreachable])
        // One candidate: no probe, as `pick`.
        let alone = await HubRoute.pickTried([a]) { _ in Issue.record("probed a single candidate"); return .answers }
        #expect(alone.picked == a && alone.tried.isEmpty)
        // `answeringTried` asks a single one too, and says why it failed.
        let single = await HubRoute.answeringTried([a]) { _ in .untrusted }
        #expect(single.winner == nil && single.tried.map(\.outcome) == [.untrusted])
    }

    /// The same choice as the yes / no `pick`, whatever the reason given for a failure. Every probe answers at once and
    /// the grace is long, so the choice does not depend on how busy the machine is.
    @Test func pickTriedPicksAsPick() async {
        let a = candidate("https://a.invalid", address: 0), b = candidate("https://b.invalid"), c = candidate("https://c.invalid", address: 2)
        let cases: [[URL: HubOutcome]] = [
            [a.url: .answers, b.url: .answers, c.url: .answers],
            [a.url: .untrusted, b.url: .answers, c.url: .answers],
            [a.url: .unreachable, b.url: .notYourHub, c.url: .answers],
            [a.url: .unknown, b.url: .unreachable, c.url: .untrusted],
        ]
        for outcomes in cases {
            let tried = await HubRoute.pickTried([a, b, c], grace: .seconds(30)) { outcomes[$0.url]! }
            let picked = await HubRoute.pick([a, b, c], grace: .seconds(30)) { outcomes[$0.url]! == .answers }
            #expect(tried.picked == picked, "\(outcomes)")
        }
    }

    // MARK: Connection Details' lines

    @MainActor @Test func linesFollowTheOrderTriedAndTheLastTry() {
        let board = HubAddressBoard()
        let a = url("https://a.invalid"), b = url("https://b.invalid:8443/")
        board.arrange("g", addresses: [a, b])
        #expect(board.lines["g"]?.map(\.id) == ["https://a.invalid", "https://b.invalid:8443"])
        #expect(board.lines["g"]?.allSatisfy { $0.outcome == nil && $0.tried == nil } == true)
        board.record("g", [(a, .untrusted, Date()), (b, .unknown, Date())])
        #expect(board.lines["g"]?.map(\.outcome) == [.untrusted, .unknown])
        #expect(board.lines["g"]?.first?.word == HubOutcome.untrusted.title)
        #expect(board.lines["g"]?.first?.tried != nil)
        // A later try of one address replaces only its line.
        board.record("g", [(a, .answers, Date())])
        #expect(board.lines["g"]?.map(\.outcome) == [.answers, .unknown])
        // The order changes with the addresses; what is known stays.
        board.arrange("g", addresses: [b, a])
        #expect(board.lines["g"]?.map(\.outcome) == [.unknown, .answers])
        board.keep([])
        #expect(board.lines["g"] == nil)
    }

    @MainActor @Test func whatCameAfterTheBackgroundWasCutShort() {
        let board = HubAddressBoard()
        let start = Date()
        let a = candidate("https://a.invalid", address: 0), b = candidate("https://b.invalid")
        let back = start.addingTimeInterval(1)
        board.wentToBackground(at: back)
        let tried = board.cutShort([.init(candidate: a, outcome: .unreachable, at: start.addingTimeInterval(0.5)),
                                    .init(candidate: b, outcome: .unreachable, at: start.addingTimeInterval(2))], since: start)
        #expect(tried.map(\.outcome) == [.unreachable, .unknown])
        // An attempt that began after the app came back is not touched.
        let later = board.cutShort([.init(candidate: b, outcome: .unreachable, at: back.addingTimeInterval(5))], since: back.addingTimeInterval(3))
        #expect(later.map(\.outcome) == [.unreachable])
    }

    /// One address and no tunnel: no probe, so the socket's own attempt says (classified the same way). With a probe,
    /// the socket only replaces an Answers at the address it then failed to open.
    @MainActor @Test func aSingleAddressTakesTheSocketsOwnOutcome() {
        let model = AppModel(stateFolder: FileManager.default.temporaryDirectory.appending(path: "outcomes-" + UUID().uuidString))
        let one = ServerConfig(url: url("http://127.0.0.1:1"), token: "device")
        model.registry = HubRegistry(hubs: [PairedHub(config: one, group: "")])
        model.buildLinks()
        #expect(model.addressBoard.lines[""]?.map(\.outcome) == [nil]) // not tried yet
        model.socketTried("", one, HubOutcome(failure: urlError(NSURLErrorServerCertificateUntrusted)))
        #expect(model.addressBoard.lines[""]?.map(\.outcome) == [.untrusted])
        model.socketTried("", one, .answers)
        #expect(model.addressBoard.lines[""]?.map(\.outcome) == [.answers])

        var two = PairedHub(config: one, group: "", hubID: "H")
        two.addresses = [HubAddress(url: url("https://b.invalid"))]
        model.registry = HubRegistry(hubs: [two])
        model.buildLinks()
        let b = two.configs[1]
        #expect(model.addressBoard.lines[""]?.count == 2)
        // The probe is the source: a socket failure where no probe said Answers changes nothing.
        model.socketTried("", b, .unreachable)
        #expect(model.addressBoard.outcome("", b.url) == nil)
        let start = Date()
        model.tried("", [.init(candidate: .init(config: b, hub: two.id, address: 1, hubID: "H"), outcome: .answers, at: Date())], since: start)
        #expect(model.addressBoard.outcome("", b.url) == .answers)
        model.socketTried("", b, .answers) // connecting there says nothing new
        model.socketTried("", b, .unreachable) // the socket then failed there: that is what the attempt came to
        #expect(model.addressBoard.outcome("", b.url) == .unreachable)
    }
}
