# Product

<!-- impeccable:product-schema 1 -->

## Platform

web

## Users

기획·QA 담당자. 사용자가 2026-09-15 관리 화면 개편의 주 사용자로 지정했다.

## Product Purpose

배포된 서비스의 검증 요청, 검사 승인, 실행 결과와 증거를 관리한다.

## Operating Context

Go 서버가 HTML, CSS, JavaScript를 제공한다. 관리 화면은 `/`, `/results`, `/scripts`, `/activity`, `/help`이며 일회성 ProofQA는 별도 서버다.

## Capabilities and Constraints

`admin`은 프로젝트·사이트 등록 및 URL 수정, 자유서술 요청에 따른 E2E 스크립트 생성·독립 검증, 수동 실행을 지원한다. 실행 상태와 화면 캡처를 먼저 보여 주며 상세 기록은 접는다. 기존 검사 상태와 승인 조건을 유지한다. `serve`는 조회 모드이며 요청 접수와 실행에는 연결된 실행기가 필요하다. 빈 이력은 미검증 상태이며 통과로 해석하지 않는다. 계정 관리, 팀 권한과 담당자 배정은 이 개편의 기능이 아니다.

## Brand Commitments

Vigil 이름과 자연스럽고 정중한 한국어를 사용한다.

## Evidence on Hand

`internal/ui`의 실제 API와 UI, 네이버를 등록한 로컬 설정. 네이버와 Example에서 검사를 실행한 증거가 있으며 검증 범위는 docs/admin-verification.md에 기록한다.

## Product Principles

- 업무 판단에 필요한 상태와 다음 행동을 먼저 보여 준다.
- 실제 결과와 아직 확인하지 못한 상태를 구별한다.
- 상세 근거와 기술 정보는 필요한 곳에서 확인한다.
- 기존 승인과 실행 범위를 보존한다.
