# Post-mortems

Incident write-ups: a bug reached somewhere it shouldn't have, and the
interesting part is *why every safety net missed it*, not the one-line fix.

Write one only when a bug is **subtle** (a careful engineer would re-derive the
mechanism the hard way), **systemic** (it escaped through a gap in tests,
tooling, or conventions rather than a typo), and **costly to rediscover**. Most
bugs fail that bar and belong in a commit message.

Every entry opens with an **Executive summary** — thirty seconds, what broke,
the mechanism, why it escaped, the durable lesson — before the detail.

The recurring theme across all three so far: **a layer reported its own success
as the whole system's success.** Publishing succeeded, so the message was
"sent". The ack succeeded, so the message was "delivered". The doc was
authoritative, so the instruction was "correct". Each fix makes the real
outcome visible instead of inferred.

| # | Title |
|---|---|
| [0001](0001-ack-without-injection.md) | Messages were acked before anyone saw them, and destroyed on reconnect |
| [0002](0002-presence-without-inbox.md) | Agents appeared on the roster with no inbox; sends reported success into a void |
| [0003](0003-retired-broker-resurrected-by-memory.md) | A corrected memory file re-started a retired service for a week |
