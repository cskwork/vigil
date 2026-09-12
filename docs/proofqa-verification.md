# ProofQA 구현과 검증 기록

2026-09-12. 사용자 승인 스펙 v0.1을 기준으로 기존 Vigil에 일회 검사 경로를 추가한다. 공개 배포는 이번 작업에 포함하지 않는다.

## 시작 상태

- Checkout: `4c9bf96f5bf7918d364e72ebf58c4cad984f1f43`.
- 작업 시작 시 변경 파일 없음.
- `go test ./...`, `go vet ./...`, `go build -o /private/tmp/vigil-baseline ./cmd/vigil`: 통과.
- 기존 실제 대상 E2E는 환경 변수로 켜는 opt-in 테스트다. 위 기본 테스트 통과가 실제 배포 앱 검증을 뜻하지 않는다.
- 저장소의 기본 대상은 `https://dev.example.com`인 예시 설정이다. 실제 업무 서비스, 계정, DB 연결은 확인되지 않았다.
- 사용자는 독립 합성 데모에서 구현·검증하고 실제 환경 연결은 후속으로 진행하도록 선택했다.

## 합성 검증 환경

- 별도 MySQL 8.4.11 인스턴스: `127.0.0.1:13317`, 임시 디렉터리의 새 데이터베이스 `proofqa_demo`.
- fixture 계정과 `SELECT`만 허용한 probe 계정을 분리했다. `SHOW GRANTS FOR CURRENT_USER()`로 probe 권한을 확인했다.
- 브라우저 검증: Ego Lite에서 ProofQA 웹을 조작하고, ProofQA runner는 별도로 소유한 Chromium을 실행한다.
- 실행 전 `vigil-chromium-*` 프로필을 사용하는 Chrome 프로세스는 0개였다.
- 개발용 데모는 독립 합성 데이터만 사용한다. 이 결과를 실제 업무 서비스 검증으로 해석하지 않는다.

## 구현 경계

Check → 수정 가능한 Contract → 일회 승인 → Attempt → 항목별 Evidence와 결과를 제공한다. Go, embedded UI, SQLite, 기존 Chromium runner를 유지하고 기존 agent 패키지에 제한된 Pi 계획 호출을 추가했다. 기존 승인, 정기 실행, 수집기, supervisor, Jira 쓰기는 ProofQA 실행에서 호출하지 않는다.

필수 기준에 유효한 FAIL이 있으면 문제 발견, 그 외 UNKNOWN이 있으면 확인 불가, 필수 기준이 하나 이상이고 모두 PASS이면 요구사항 충족이다. 기준과 타입, 실제 관찰, 근거 유무를 코드로 비교한다. 사람의 결과 수용은 기술 판정을 바꾸지 않는다.

## 검증 책임과 최소 근거

구현 담당은 도메인·저장·runner·API의 의미 있는 실패 사례를 자동 테스트로 확인한다. 조정 담당은 전체 회귀, 실행 결과, 실제 브라우저의 사용자 흐름·반응형 화면을 독립 확인한다. 같은 근거를 중복 생성하지 않고 코드나 환경이 바뀐 부분만 다시 확인한다.

| 주장 | 필요한 근거 | 현재 상태 |
|---|---|---|
| 기존 사용 방식 유지 | 전체 Go 테스트, vet, build | 변경 후 전체 명령 통과 |
| 기준 수정과 승인을 분리 | revision 충돌, 불변 snapshot 테스트 | 자동 테스트 통과. 등록 범위 변경도 별도 hash로 거절 |
| 중복 클릭은 한 번만 실행 | 동일 idempotency key 및 다른 payload 반례 | 동시 HTTP 요청 2건이 같은 attempt 반환. 다른 payload는 409 |
| UNKNOWN을 PASS로 바꾸지 않음 | 타입·누락·집계·실패 후 미수행 기준 테스트 | 자동 테스트 및 실제 DB 미연결·취소·복구 실행 통과 |
| UI/API가 같아도 기대값과 다르면 실패 | 통제 앱에서 실제 Chromium과 API 비교 | 실제값 둘 다 빈 문자열, 승인 기대값은 정답: UI/API 모두 FAIL |
| 다른 대상·시점 응답 배제 | entity/action/session 및 누락 필드 반례 | observer 단위 테스트 통과. 실제 API 근거에도 요청 ID·시각·entity·session 기록 |
| 선택적 DB 조회와 기록 보존 | named probe 전후 집합, 미연결 반례 | MySQL 직접 조회에서 B의 기록 삭제 FAIL, 미연결 UNKNOWN |
| 승인 범위 밖 요청 차단 | redirect 및 비허용 origin 반례 | 실제 Chromium에서 허용 origin의 302를 따라가는 금지 origin 요청 0회, UNKNOWN과 차단 사유, owned browser 종료 확인 |
| 취소·재시작 시 쓰기 재전송 없음 | 중단 journal, 재시작 상태, owned browser 종료 | 실제 취소와 SIGKILL 뒤 복구 통과. 재실행 0, 남은 Chromium·시도 잠금 0 |
| 승인에 정기 실행 부수효과 없음 | proof jobs와 legacy schedule/approval 경계 테스트 | 실행 DB에는 PROOF_PLAN/PROOF_RUN만 존재. scenario/feature/incident 생성 0 |
| 인증·증거 접근 제어 | 인증 누락, 다른 사용자, CSRF, 경로 탈출 반례 | 고정 gateway 주체·토큰·CSRF·manifest·만료 경계 자동 테스트 통과. 조직 사용자별 권한은 미검증 |
| 요청부터 결과까지 사용할 수 있음 | 실제 브라우저 생성·승인·재조회·재실행·복사, desktop/mobile | Ego에서 실제 흐름 통과. 독립 Chromium의 1280×900 및 390×844 화면·대비·버튼 크기·가로 넘침 검사 통과 |
| 수정 전후 비교를 과장하지 않음 | baseline 조건, 버전 누락·변경·만료 반례 | 실제 A의 FAIL → C의 PASS 비교 통과. 버전 변경·부적합 baseline 거절 자동 테스트 통과 |
| 실제 업무 환경에서 동작 | 운영자가 승인한 QA URL·계정·DB에서 실행 | Not proven. 연결 정보 필요 |
| 처음 사용자 5명 중 4명 성공 | 5명의 독립 첫 사용 관찰 | Not proven. 참가자 필요 |

## 필수 반례 추적

사용자 스펙 §16.1의 20개 반례를 다음 그룹으로 추적한다.

1. 업무 판정: UI/API 불일치, 같은 잘못된 값, DB 기록 삭제, DB 미연결, 다른 학생·이전 응답, 누락/null/빈 문자열/0.
2. 실행 안정성: 지연 저장, 승인 뒤 수정, 중복 클릭·새로고침, 취소·재시작, 인증 만료.
3. 증거 정직성: 이전 배포 없음, 실행 중 버전 변경, 결과 수용 불변, 증거 만료.
4. 권한: 페이지 지시로 계약 변경 금지, 정기 실행 부수효과 없음, 외부 redirect 차단, 증거 ID 추측 거절, assertion 삭제 수리 금지.

실제 실행, 통제된 개발용 앱 실행, mock 기반 단위 테스트를 구분해 기록한다. 미연결 기능은 미지원 또는 확인 불가로 노출한다. 작은 평가 세트의 통과를 범용 환경의 정확도 보장으로 표현하지 않는다.

## 실제 합성 실행 영수증

각 실행은 독립 generation으로 초기화한 같은 QA fixture 규칙을 사용했다. 아래 ID는 해당 실행의 SQLite attempt ID다.

| 사례 | attempt ID | 화면 / API / DB | 전체 |
|---|---|---|---|
| 정상 C | `46a3b72564836c5894a70517d42208cc` | PASS / PASS / PASS | PASS |
| API 값이 남는 A | `59c21926b0ec509e2736a9fd57f7c1b5` | PASS / FAIL / PASS | FAIL |
| 기록이 삭제되는 B | `5e689ba45638d39e1d088aa5da89d31d` | PASS / PASS / FAIL | FAIL |
| DB 연결 없음 | `2d4e906421a159523ce4fda4ed2e85f9` | PASS / PASS / UNKNOWN | INCOMPLETE |
| UI/API의 같은 잘못된 값 | `38b4c1ee8c0ffc740feea3105a03763b` | FAIL / FAIL / PASS | FAIL |
| A와 비교한 C | `f0fa60ed68d9ae2377d6d2fb6a7bdfb6` | PASS / PASS / PASS | PASS, 안정된 A/C 버전 비교 |
| 저장 전 취소 | `086ccd99c61ef1d7ffc0349f621e71b1` | 미완료 | CANCELLED / INCOMPLETE, 값 0 유지 |
| 서버 강제 종료 후 복구 | `6115bf62cbf712ffbd86b0abef6fdf2d` | 미완료 | INTERRUPTED / INCOMPLETE, 쓰기 재전송 0 |

최종 브라우저 사용 흐름의 Check는 `9750a6ba622d6608980905ff16422d6a`다. 입력 후 기준 생성·실행 버튼으로 `587e10acf4fbdf5b69192e662dda203e`가 생성됐고, 새로고침해도 같은 attempt가 유지됐다. 같은 기준 재실행은 `1986bacb2e94196f40910f3ecbd2eac3`를 생성했다. 두 실행 모두 PASS이며 결과 복사도 완료됐다. 재실행 버튼의 첫 자동 조작은 드라이버의 pointer interception 오류로 다시 관찰한 후 재시도했다. 이를 사용자 최초 성공률 측정으로 집계하지 않는다.

SQLite backup API로 실행 DB의 별도 사본을 만들었고 `PRAGMA integrity_check`는 `ok`였다. 마지막 프로세스 검사에서도 `vigil-chromium-*` 프로필을 쓰는 Chrome 프로세스는 0개였다. peak RSS의 연속 측정과 24개 균형 평가 세트는 수행하지 않았으므로 성능·일반 정확도 주장을 하지 않는다.

리다이렉트 반례는 `PROOF_BROWSER_E2E=1 go test ./internal/proof -run TestProofBrowserRejectsRedirectOutsideScope -count=1 -v`로 재실행할 수 있다. 테스트가 독립 서버 두 개와 SQLite, Chromium을 만들고 정리하며 회사 서비스나 사용자 브라우저를 사용하지 않는다.

로컬 검증 영수증과 최종 화면은 `.vigil/proofqa-acceptance-20260912/`에 보존했다. 이 디렉터리는 git에서 제외되며 위 표와 자동 테스트 소스가 저장소에 남는 검증 기록이다.

## 모델 평가와 남은 범위

Pi에 등록된 `openai-codex/gpt-6-astra`로 동일 합성 요청을 3번 계획했다. 세 번 모두 유효한 계약을 만들지 못했다. 같은 제한 옵션의 최소 입력으로 확인한 제공자 오류는 `Codex error: The usage limit has been reached`였다. 모델 사용량은 해당 오류 응답에서 0으로 보고됐다. 사용 한도 오류를 UI에 구분해 표시하는 파서는 이 실제 응답 형태로 자동 테스트했다. 실제 모델의 성공률·기준 생성 편차는 **Not proven**이며 한도가 회복된 뒤 다시 평가해야 한다.

현재 구현은 운영자가 등록한 DOM/API/DB 흐름에 한정한다. 새 화면의 자동 브라우저 탐색, iframe/popup 흐름, 조직별 다중 사용자 로그인은 제공하지 않는다. 고정된 게이트웨이 주체를 사용하는 인증 경계와 단일 로컬 운영자 경계는 구현했지만 공개 인터넷 운영이나 조직 SSO 검증을 대신하지 않는다. 실제 업무 환경 연결과 5명의 첫 사용 관찰은 사용자 선택에 따라 후속으로 남겼다.

이 문서는 커밋·푸시 전후의 검증 근거를 함께 기록한다. 공개 배포는 수행하지 않았다. 실행 방법과 제약은 [ProofQA 실행 안내](proofqa.md)를 참고한다.
