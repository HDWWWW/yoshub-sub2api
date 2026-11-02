# Account retry defaults and refill controls

- The built-in same-account retry default is 3 additional attempts (0–10).
- Accounts toolbar: Default retries persists the server default for newly created and manually imported accounts. Explicit imported values, including zero and legacy pool-mode values, take precedence. Existing account overrides are preserved.
- Automatic refill: Delivery settings includes Same-account retries. The default is 3, independently configurable per rule. Accepted jobs retain their delivery snapshot; manual refill uses the same saved rule.
- No schema migrations or bulk account updates. Existing records without overrides use the new built-in fallback. Protocol repair requests and cross-account failover remain separate.
