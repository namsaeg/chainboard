# chainboard

검열에 저항하는 탈중앙 게시판입니다. 글은 전용 블록체인에, 첨부 파일은 채굴자들이 나눠 저장하는 P2P 저장소에 올라갑니다. 채굴력은 **얼마나 많은 데이터를 실제로 저장하고 있는가**로 정해집니다.

> **0.1.0은 테스트넷 전용 프로토타입입니다.** 보안 감사를 받지 않았으니 실제 가치를 가진 코인으로 쓰지 마세요.

## 빠른 시작 (Windows)

Go 1.22 이상이 필요합니다.

```powershell
git clone https://github.com/namsaeg/chainboard
cd chainboard
go build -o chainboard.exe ./cmd/chainboard

# 혼자 써 보기: 빠른 로컬 개발망에서 채굴하며 실행
.\chainboard.exe -network devnet -mine -pledge-gb 1
```

브라우저에서 <http://127.0.0.1:7780> 을 열면 게시판 화면이 나옵니다. 채굴로 코인이 생기면 글을 쓸 수 있어요(잠긴 채굴 보상도 무료 차선 예치로 인정됩니다).

여러 노드를 연결하려면 다른 PC에서 `-seeds 192.168.0.10:7700` 처럼 첫 노드의 주소를 넣으세요.

### 주요 옵션

| 옵션 | 기본값 | 설명 |
|---|---|---|
| `-network` | `testnet` | `testnet` 또는 `devnet`(빠른 로컬 시험용) |
| `-datadir` | 사용자 설정 폴더 | 블록, 지갑 키, 저장 데이터 위치 |
| `-listen` | `:7700` | P2P 수신 주소 |
| `-seeds` | 없음 | 처음 연결할 노드들 (`host:port,host:port`) |
| `-http` | `127.0.0.1:7780` | 웹 UI·API 주소. 지갑을 제어하므로 localhost에서 바꾸지 마세요 |
| `-mine` | `false` | 데이터를 저장하고 채굴 |
| `-pledge-gb` | `100` | 저장에 내놓을 디스크 용량(GB) |
| `-send-delay` | testnet 5분, devnet 0 | 내 트랜잭션을 내보내기 전 최대 무작위 지연 |

지갑 키는 `<datadir>/<network>/wallet.key` 에 평문으로 저장됩니다(0.1.0은 암호화하지 않습니다). 백업하고 남에게 보여 주지 마세요.

## 무엇이 들어 있나

- **저장 기반 합의**: Arweave식 무작위 청크 증명. VDF(순차 SHA-256)가 초당 도전 횟수를 제한하므로 CPU가 아니라 저장량이 채굴력이 됩니다. 청크는 채굴자 주소·위치마다 고유하게 인코딩(packing)되어 한 사본을 여러 주소로 우려먹을 수 없습니다.
- **보존 등급**: 첨부는 1개월 / 1~100년 / 영구 중 선택. 누구나 비용을 내고 연장할 수 있습니다. 기간제 데이터는 만료 시점별 버킷에 모였다가 통째로 만료됩니다. 글(텍스트)은 항상 영구입니다.
- **수수료 차선**: 무료 차선(예치 기반 할당, 텍스트 전용, 용량의 30%)과 수수료 차선(EIP-1559식 기본 수수료 소각). 같은 글 반복은 비용이 두 배씩 오릅니다.
- **경제**: 업로드 수수료는 기금(영구)·에스크로(기간제)로 들어가 저장 노드에 나눠 지급됩니다. 발행은 6개월 증가 → 4년 반감 → 저장 용량 비율 피드백 꼬리 발행. 모든 가격은 "원시 1GB를 1년 저장" 단위로 매겨지고, 체인이 관측한 저장 가격(30일 이동평균, 하루 ±5%)으로 코인 환산됩니다.
- **이탈 방지**: 채굴 수입의 75%는 180일 동안 풀리며, 저장 증명을 계속해야 받을 수 있습니다. 3에포크 연속 증명이 없으면 몰수되어 복구 보상 풀로 갑니다.
- **네트워크**: 상호 인증 TLS 1.3(겉보기에는 일반 HTTPS), Dandelion++ 전파, 내 트랜잭션은 무작위 지연 후 순서대로 묶어 발송. 노드 키와 지갑 키를 분리해 네트워크 신원이 계정과 연결되지 않습니다.
- **악성 파일 방어**: 업로드 이미지는 PNG/JPEG/GIF만 받고 픽셀부터 다시 인코딩해 메타데이터와 숨은 페이로드를 지웁니다. 브라우저에는 허용된 이미지 형식만 제공합니다.
- **서명**: Ed25519 (결정적 논스, 상수 시간 구현, 빠른 검증).

자세한 규칙은 [DESIGN.md](DESIGN.md), 이번 버전의 범위와 한계는 [CHANGELOG.md](CHANGELOG.md)에 있습니다.

## 개발

```sh
go vet ./...
go test ./...          # 노드 3개를 실제 TLS로 연결하는 통합 테스트 포함
go test -race ./internal/node ./internal/miner
```

외부 의존성이 없습니다(Go 표준 라이브러리만 사용).

```
cmd/chainboard     실행 파일 (노드 + 웹 UI)
internal/params    네트워크 파라미터 (합의 상수)
internal/cryptox   해시, Ed25519 키, 주소, Balloon 해싱
internal/codec     정규 바이너리 인코딩
internal/merkle    머클 트리
internal/chunk     256KiB 청크 / 8KiB 하위 청크와 데이터 루트
internal/pack      채굴자별 고유 인코딩
internal/vdf       순차 해시 체인
internal/types     트랜잭션과 블록
internal/state     상태와 상태 전이 함수 (합의 규칙 전부)
internal/chain     블록 보관, 가장 무거운 체인 선택, 재구성
internal/mempool   대기 트랜잭션
internal/store     청크와 패킹된 사본 저장
internal/miner     파티션 할당·패킹·채굴·에포크 증명
internal/p2p       TLS 전송, 가십, Dandelion++, 동기화, 청크 교환
internal/media     이미지 검사와 재인코딩
internal/node      전체 조립, 지갑, 지연 발송, 글 색인
internal/api       로컬 HTTP API와 웹 UI
```
