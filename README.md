# 脚本
## 目前有的功能
- **部署质押合约**
    ``` bash
    go run ./cmd/contract/depositContract
- **部署退出合约**
    ``` bash
    go run ./cmd/contract/exitContract
- **测试带错误BLS签名的质押操作**
    ```bash
    go run ./cmd/deposit-test/deposit-sig-tamper
- **批量发送质押请求
    ```bash
    并发（按顺序输出）
    go run ./cmd/deposit-test/deposit-batch \
  -json ./deposit-data.json \
  -rpc http://127.0.0.1:8545 \
  -contract 0x5FbDB2315678afecb367f032d93F642f64180aa3 \
  -mode concurrent \
  -workers 8 \
  -amount-eth 32 \
  -limit 20
  -no-wait
    ```bash
    严格顺序逐条发送
    go run ./cmd/deposit-test/deposit-batch \
  -json ./deposit-data.json \
  -rpc http://127.0.0.1:8545 \
  -contract 0x5FbDB2315678afecb367f032d93F642f64180aa3 \
  -mode sequential \
  -amount-eth 32 \
  -limit 20

