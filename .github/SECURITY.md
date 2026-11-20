# Security policy

## Supported versions

KingresDB has not made a release yet, and no version is supported in the sense of receiving security updates. Security fixes are made on the development branches:

| Branch | PostgreSQL | Status |
|---|---|---|
| `kingres-18` | 18 | Development branch and default branch. Fixes are made here first |
| `kingres-19` | 19 | Receives the KingresDB code from `kingres-18` by synchronisation |

When releases exist, this table will list the supported ones. The project states openly which parts are built but not yet independently reviewed; see [`kingres/README.md`](../kingres/README.md) for the state of every phase. Before a release, assume that a part described there as not reviewed, or as having open findings, is not safe to rely on.

## Reporting a vulnerability

Please report a vulnerability in KingresDB privately. Do not open a public issue, pull request or discussion for it.

Use GitHub's private vulnerability reporting: open the **Security** tab of the repository, choose **Report a vulnerability**, and fill in the form. Only the maintainers can see the report.

Please include:

- what the problem is and which component it affects (an extension under `contrib/kingres_*`, `kingresd`, the REST API, the web console, the verifier, the packaging);
- the branch or commit and the PostgreSQL major you tested on;
- the steps to reproduce it, with the smallest SQL, request or input that shows it;
- what an attacker gains (for example reading or changing data, bypassing the ledger, running code, denying service) and what the attacker needs first (a role, network access, a key);
- any fix or mitigation you already know of.

Do not include real credentials, personal data or private keys of other people in a report.

## What to expect

- The maintainers will acknowledge the report and ask questions if something is unclear.
- They will reproduce the problem, decide how serious it is, and tell you what they decided.
- A fix is made with a test that fails without it, on the development branches. A report that describes a defect in a part that is documented as not yet reliable is still welcome and will be handled.
- You will be told when the fix is available. If you want to be credited, say so in the report; the maintainers will not name you if you do not.
- Please give the maintainers reasonable time to fix the problem before you publish details.

## Vulnerabilities in PostgreSQL itself

KingresDB contains the PostgreSQL source. A vulnerability in PostgreSQL code that is not changed by KingresDB should be reported to the PostgreSQL security team, following <https://www.postgresql.org/support/security/>. If you are not sure whether a problem is in PostgreSQL or in KingresDB, report it here and the maintainers will forward it where it belongs.
