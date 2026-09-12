# ProofQA 실행 안내

ProofQA는 등록한 배포 대상에서 요청별로 계약을 작성하고, 사용자가 한 번 승인한 범위만 실행합니다. 기존 `loop`, `approve`, 수집기, 정기 실행 기능은 시작하지 않습니다. 애플리케이션 저장소를 등록할 필요가 없습니다.

화면은 업무용 검증 콘솔로 구성됩니다. 홈에서는 새 검증 요청과 최근 결과를 함께 보고, Check 상세에서는 승인 범위, 실행 단계, 항목별 근거, Attempt 이력과 감사 정보를 같은 화면에서 확인합니다.

## 독립 데모

먼저 두 실행 파일을 빌드합니다.

```sh
go build -o /tmp/vigil ./cmd/vigil
go build -o /tmp/proof-demo ./cmd/proof-demo
```

첫 번째 터미널에서 데모를 실행합니다. `C`는 정상 구현, `A`는 저장 후 API가 이전 값을 반환하는 구현, `B`는 기존 이력을 삭제하는 구현입니다. 같은 주소에서 프로세스를 다시 시작하여 버전을 바꿀 수 있습니다.

```sh
/tmp/proof-demo --addr 127.0.0.1:8790 --version C --registry /tmp/proof-registry.json
```

두 번째 터미널에서 ProofQA를 실행합니다.

```sh
/tmp/vigil proof --registry /tmp/proof-registry.json \
  --db /tmp/proofqa/state.db --evidence /tmp/proofqa/evidence \
  --addr 127.0.0.1:8788 --local-operator
```

브라우저에서 `http://127.0.0.1:8788`을 엽니다. 다음 요청은 등록된 데모 계약으로 변환됩니다.

> 답을 지우면 화면과 API의 현재 답은 비어 있어야 하고, 기존 제출 기록은 유지되어야 합니다.

`확인 기준 만들기`를 누르고 기준을 검토한 뒤 실행합니다. 데모 계약은 빈 문자열과 이력 보존이라는 등록된 정의를 사용합니다. 다른 요청을 같은 계약으로 임의 변환하지 않습니다. 일반 요청은 등록된 대상 정의와 Pi 계획 도구가 필요합니다. 도구가 없으면 지원하지 않는 요청으로 표시합니다. 모델 제안은 승인이나 판정이 아닙니다.

한 사람이 기획·QA·개발 관점의 확인을 모두 맡아도 역할을 바꿀 필요가 없습니다. 요청을 작성한 같은 계정이 기준을 수정하고 실행하며, 결과에 `확인 완료`, `보류`, `반려` 판단을 별도로 남깁니다. 이 판단은 기술 결과를 바꾸지 않습니다.

## 선택 사항: 실제 MySQL 관찰

데모 앱의 `PROOF_DEMO_MYSQL_DSN`은 **독립 테스트 데이터베이스**의 fixture 쓰기 계정입니다. ProofQA의 `PROOF_MYSQL_DSN`은 같은 데이터베이스의 **SELECT 전용 계정**입니다. 실제 업무 데이터베이스의 계정을 넣지 마세요. DSN 값은 JSON 레지스트리에 저장하지 않고 환경 변수로 전달합니다.

```text
PROOF_DEMO_MYSQL_DSN=fixture_user:password@tcp(127.0.0.1:3306)/isolated_proofqa
PROOF_MYSQL_DSN=reader_user:password@tcp(127.0.0.1:3306)/isolated_proofqa
```

데모 앱은 `proof_history` 테이블을 만들고, 승인된 실행마다 `qa-record` fixture를 초기화합니다. MySQL이 없으면 화면과 API 검사는 실행되며 이력 기준은 `UNKNOWN`, 종합 결과는 `INCOMPLETE`입니다. 이 기준을 자동 삭제하지 않습니다.

각 named probe는 레지스트리에 고정된 SELECT 한 개와 entity 파라미터 한 개만 사용합니다. 읽기 전용 트랜잭션, 2초 제한, 최대 100행, 동시 실행 1개를 적용합니다. 키와 필드 집합을 정렬해 저장 전후를 비교합니다. 연결 실패, 중복 키, 열 불일치와 행 제한 초과는 `UNKNOWN`입니다.

## 등록 대상과 계약

레지스트리 JSON은 `targets`, 선택적인 `probes`, `pi_command`, `pi_model`로 구성됩니다. 데모가 출력하는 JSON이 전체 예제입니다. CLI는 시작 전에 등록된 주소, 동작 종류, persona와 observer 참조를 검증합니다.

- 대상은 `base_url`, `environment`, `policy_version`, 정확한 origin/method/path/query 허용 목록을 갖습니다.
- persona는 계정 잠금 이름, 등록된 준비 단계, `secret_env` 참조를 갖습니다. 비밀번호를 단계에 직접 적지 않습니다.
- 쓰기는 dev/development/qa/test 환경의 QA fixture와 등록된 준비 URL이 필요합니다. 준비 응답의 entity와 attempt가 일치해야 합니다.
- action은 등록된 DSL 단계입니다. 실행 요청이나 모델 출력에서 임의 스크립트·URL·SQL을 받을 수 없습니다.
- observer는 DOM 속성, 정확하게 연결된 API 응답, 또는 고정 MySQL probe입니다. 저장 동작의 `write_api`와 다시 읽는 GET을 모두 지정해야 저장 후 조회를 입증할 수 있습니다.
- 기대값은 `{ "present": true, "data": "" }`처럼 저장합니다. 없음, null, 빈 문자열, 0과 false를 구분합니다. 출처는 요청의 정확한 인용 또는 등록된 definition입니다.

필수 기준은 1~5개입니다. 필수 FAIL이 하나라도 있으면 FAIL입니다. 그렇지 않고 UNKNOWN이 있거나 필수 기준이 없으면 INCOMPLETE입니다. 필수 기준이 모두 PASS일 때 PASS입니다. 선택 기준의 결과는 별도로 표시합니다.

## 인증과 범위

`--local-operator`는 명시적인 단일 로컬 운영자 모드이며 literal loopback 주소에만 바인딩합니다. Host와 요청 출발 주소도 loopback인지 확인합니다. 외부 바인딩은 인증 설정이 없으면 거부합니다.

한 사용자가 원격 서버에 로그인하려면 [사용자 예제](../examples/proof/users.json)에 계정 이름, 팀과 비밀번호 환경 변수 이름을 지정합니다. JSON에 비밀번호를 넣지 않습니다. 비밀번호는 16자 이상이며 서버 환경 변수로만 전달합니다. 외부 주소에서는 정확한 HTTPS origin이 필요합니다.

```sh
PROOF_LOGIN_PASSWORD='운영 환경의 긴 비밀번호' /tmp/vigil proof \
  --registry /srv/proof/registry.json --users /srv/proof/users.json \
  --public-origin https://proof.example.com --addr 127.0.0.1:8788
```

로그인 세션은 HttpOnly·SameSite=Strict 쿠키를 쓰며 8시간 뒤 만료됩니다. 변경 요청은 세션별 CSRF 토큰과 정확한 Origin을 확인합니다. 1분 동안 로그인 실패가 5회 쌓이면 잠시 차단합니다. 서버를 다시 시작하면 다시 로그인합니다.

외부 서비스는 신뢰할 수 있는 인증 게이트웨이가 `--gateway-token-env`로 지정한 32자 이상의 bearer token을 삽입하도록 구성할 수 있습니다. `--gateway-actor`는 그 token의 고정된 인증 주체입니다. 클라이언트가 보낸 사용자 이름 헤더는 신뢰하지 않습니다. 이 기능은 제품 자체의 여러 사용자 로그인 시스템이 아닙니다. 사용자별 권한·팀 분리와 조직 SSO 연동은 실제 게이트웨이에서 추가 검증해야 합니다. 프록시의 원본 Host/Origin과 TLS 종료 구성을 확인해야 합니다.

모든 변경 API는 `/api/proof/session`의 CSRF token을 `X-Proof-CSRF`로 요구합니다. 증거는 해당 attempt manifest에 연결된 파일만 제공하며 기존 legacy evidence 경로는 제공하지 않습니다.

화면 동작에서 저장 후 API 재조회가 발생하지 않는 대상은 운영자가 network observer에 `direct_read: true`를 지정할 수 있습니다. 이 옵션은 같은 브라우저 세션에서 레지스트리에 정확히 등록한 GET 한 건만 실행합니다. 임의 URL이나 모델이 찾은 endpoint는 호출하지 않습니다.

브라우저 요청은 등록된 exact scope로 차단합니다. 현재 실행기는 일반 페이지 안의 DOM/API 흐름을 지원합니다. iframe, popup, worker, object는 삽입한 CSP로 금지합니다. 이를 요구하는 대상은 지원되지 않으며 대상 환경에서 별도 검증 없이 지원한다고 간주하면 안 됩니다. 실제 회사 사이트의 복잡한 로그인, iframe/SSO, 네트워크 정책은 별도 통합 승인 대상입니다.

## API

| 메서드 | 경로 | 내용 |
|---|---|---|
| POST | `/api/proof/login`, `/api/proof/logout` | 선택한 쿠키 로그인 모드의 세션 시작 / 종료 |
| GET | `/api/proof/session` | 인증 주체와 CSRF token |
| GET | `/api/proof/registry` | 공개 가능한 등록 대상 |
| POST/GET | `/api/proof/checks` | 요청 생성 / 최신순 cursor 목록 |
| GET | `/api/proof/checks/{id}` | 체크와 시도 목록 |
| PATCH | `/api/proof/checks/{id}/contract` | `row_version`, `contract`, 선택적 `removal_reason` |
| POST | `/api/proof/checks/{id}/plan` | 같은 Check에서 중요한 질문에 답하거나 기준 생성을 다시 시도 |
| POST | `/api/proof/checks/{id}/attempts` | `revision`, `contract_hash`, `registry_hash`, `scope`, `idempotency_key`, 선택적 `baseline` |
| GET | `/api/proof/attempts/{id}` | 진행과 불변 결과 |
| POST | `/api/proof/attempts/{id}/cancel` | 이후 동작 중단 |
| POST | `/api/proof/attempts/{id}/disposition` | 별도 처리 상태·이유 |
| GET | `/api/proof/attempts/{id}/evidence/{evidence}` | 연결된 JSON 증거 |
| GET | 같은 증거 경로 + `?format=image` | 가능한 경우 기준 영역 이미지 |
| GET | 같은 증거 경로 + `?format=before` | 가능한 경우 변경 전 기준 영역 이미지 |

조회 JSON은 ETag를 제공하며 If-None-Match가 일치하면 304를 반환합니다. registry_hash는 화면에서 검토한 등록 범위를 승인에 묶습니다. 동작이나 fixture 등의 등록 내용이 바뀌면 409로 재검토를 요구합니다.

동일 키·동일 요청은 같은 attempt를 반환합니다. 동일 키의 다른 요청, 오래된 revision/hash는 409입니다. 동시에 한 proof attempt만 허용하며 추가 실행은 429입니다. 형식 오류는 400, 접근 거부는 403, 등록 범위·계약 오류는 422입니다.

## 중단, 증거와 보관

체크와 계획 작업, 시도와 실행 작업은 각각 SQLite 트랜잭션으로 함께 생성합니다. 기존 worker와 lease 회수는 proof 작업을 가져가지 않습니다. 서비스 전체와 계정/entity 잠금을 사용합니다. 실행 제한은 3분이며 종료할 때 자신이 시작한 Chromium만 정리합니다. 서버가 중단되면 시도를 자동 재실행하지 않고 INTERRUPTED로 남깁니다. 재시작 시 PID와 고유 profile 경로를 모두 확인한 소유 Chromium만 정리합니다. 서비스 lease가 남은 비정상 종료 직후에는 최대 30초 후 다시 시작해야 할 수 있습니다.

원본 JSON/기준 영역 이미지는 기본 7일, 결과 요약은 30일 보관합니다. `--raw-retention-days`와 `--summary-retention-days`로 변경할 수 있습니다. raw DB 전후 행 집합과 request metadata는 원본 artifact에만 저장합니다. SQLite 요약은 판정에 필요한 비교값과 manifest를 보관합니다. 없거나 만료된 파일은 접근 시 410으로 표시합니다. 알려진 자격 정보와 credential 이름의 필드는 원본을 쓰기 전에 가립니다. 입력 필드나 알려진 비밀값을 포함한 DOM 영역은 이미지를 생성하지 않습니다.

저장 전후 비교는 같은 시도에서의 before-action baseline입니다. 이전 배포와 비교하려면 별도 baseline attempt를 승인 요청에 지정해야 합니다. 동일 계약·대상·환경·policy·fixture 조건의 기존 FAIL과 양쪽에서 실제 관찰한 서로 다른 안정된 배포 버전이 있어야 수정 확인을 표시합니다. 근거가 없으면 `Not proven`입니다.

## 검증과 현재 한계

기본 검증은 `go test ./...`, `go vet ./...`입니다. UI와 실제 Chromium 통합 검사는 `internal/proof/ui_e2e_test.go`의 opt-in 환경 변수를 참고하세요. 데모 A/B/C와 실제 격리 MySQL에서의 실행 증거는 [검증 기록](proofqa-verification.md)에 정리합니다.

현재 지원하는 JSON 필드 경로는 객체의 점 표기입니다. 배열 경로, 임의 실행 스크립트, LLM SQL, 자동 수정, 정기 실행, 외부 게시와 production 쓰기는 제공하지 않습니다. 사용성 기준은 기획·QA·개발 확인을 한 계정이 수행하는 한 사용자 흐름으로 정했고 합성 환경에서 검증했습니다. 실제 회사 대상 검증은 수행하지 않았으며 독립 데모 결과를 그 근거로 대신하지 않습니다. Pi는 등록된 observer만 선택하는 단일 tool-free JSON 제안 호출을 지원합니다. 계약 생성 전에 등록된 시작 화면 한 곳의 컨트롤 구조만 읽으며 최대 두 Document URL을 허용합니다. 서비스 전체를 자동 탐색하지 않습니다. 실제 모델 계정·provider 동작은 배포 환경에서 확인해야 합니다.
