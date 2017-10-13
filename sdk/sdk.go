package sdk

import (
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"strconv"

	"DNA/account"
	. "DNA/common"
	"DNA/common/config"
	. "DNA/core/asset"
	"DNA/core/contract"
	"DNA/core/signature"
	"DNA/core/transaction"
)

type BatchOut struct {
	Address string
	Value   string
}

// sortedAccounts used for sequential constructing program hash for verification
type sortedAccounts []*account.Account

func (sa sortedAccounts) Len() int      { return len(sa) }
func (sa sortedAccounts) Swap(i, j int) { sa[i], sa[j] = sa[j], sa[i] }
func (sa sortedAccounts) Less(i, j int) bool {
	if sa[i].ProgramHash.CompareTo(sa[j].ProgramHash) > 0 {
		return false
	} else {
		return true
	}
}

type sortedCoinsItem struct {
	input *transaction.UTXOTxInput
	coin  *account.Coin
}

// sortedCoins used for spend minor coins first
type sortedCoins []*sortedCoinsItem

func (sc sortedCoins) Len() int      { return len(sc) }
func (sc sortedCoins) Swap(i, j int) { sc[i], sc[j] = sc[j], sc[i] }
func (sc sortedCoins) Less(i, j int) bool {
	if sc[i].coin.Output.Value > sc[j].coin.Output.Value {
		return false
	} else {
		return true
	}
}

func sortCoinsByValue(coins map[*transaction.UTXOTxInput]*account.Coin) sortedCoins {
	var coinList sortedCoins
	for in, c := range coins {
		tmp := &sortedCoinsItem{
			input: in,
			coin:  c,
		}
		coinList = append(coinList, tmp)
	}
	sort.Sort(coinList)
	return coinList
}

func MakeRegTransaction(wallet account.Client, name string, value string) (*transaction.Transaction, error) {
	admin, err := wallet.GetDefaultAccount()
	if err != nil {
		return nil, err
	}
	issuer := admin
	asset := &Asset{name, name, byte(MaxPrecision), AssetType(Token), UTXO}
	transactionContract, err := contract.CreateSignatureContract(admin.PubKey())
	if err != nil {
		fmt.Println("CreateSignatureContract failed")
		return nil, err
	}
	fixedValue, err := StringToFixed64(value)
	if err != nil {
		return nil, err
	}
	tx, _ := transaction.NewRegisterAssetTransaction(asset, fixedValue, issuer.PubKey(), transactionContract.ProgramHash)
	txAttr := transaction.NewTxAttribute(transaction.Nonce, []byte(strconv.FormatInt(rand.Int63(), 10)))
	tx.Attributes = make([]*transaction.TxAttribute, 0)
	tx.Attributes = append(tx.Attributes, &txAttr)
	if err := signTransaction(issuer, tx); err != nil {
		fmt.Println("sign regist transaction failed")
		return nil, err
	}

	return tx, nil
}

func MakeIssueTransaction(wallet account.Client, assetID Uint256, address string, value string) (*transaction.Transaction, error) {
	admin, err := wallet.GetDefaultAccount()
	if err != nil {
		return nil, err
	}
	programHash, err := ToScriptHash(address)
	if err != nil {
		return nil, err
	}
	fixedValue, err := StringToFixed64(value)
	if err != nil {
		return nil, err
	}
	issueTxOutput := &transaction.TxOutput{
		AssetID:     assetID,
		Value:       fixedValue,
		ProgramHash: programHash,
	}
	outputs := []*transaction.TxOutput{issueTxOutput}
	tx, _ := transaction.NewIssueAssetTransaction(outputs)
	txAttr := transaction.NewTxAttribute(transaction.Nonce, []byte(strconv.FormatInt(rand.Int63(), 10)))
	tx.Attributes = make([]*transaction.TxAttribute, 0)
	tx.Attributes = append(tx.Attributes, &txAttr)
	if err := signTransaction(admin, tx); err != nil {
		fmt.Println("sign issue transaction failed")
		return nil, err
	}
	return tx, nil
}

func getTransferTxnPerOutputFee(outputNum int) (Fixed64, error) {
	var txnFee Fixed64
	var err error
	if v, ok := config.Parameters.TransactionFee["Transfer"]; ok {
		if v == -1 {
			// TODO: calculate transaction fee by using transaction size
		} else {
			// get transaction fee from configuration file, precision .4
			txnFee, err = StringToFixed64(strconv.FormatFloat(v, 'f', 4, 64))
			if err != nil {
				return Fixed64(0), errors.New("invalid transaction fee")
			}
		}
	}

	return txnFee / Fixed64(outputNum), nil
}
func MakeTransferTransaction(wallet account.Client, assetID Uint256, batchOut ...BatchOut) (*transaction.Transaction, error) {
	//TODO: check if being transferred asset is System Token(IPT)
	outputNum := len(batchOut)
	if outputNum == 0 {
		return nil, errors.New("nil outputs")
	}

	// get main account which is used to receive changes
	mainAccount, err := wallet.GetDefaultAccount()
	if err != nil {
		return nil, err
	}
	perOutputFee, err := getTransferTxnPerOutputFee(len(batchOut))
	if err != nil {
		return nil, err
	}

	var expected Fixed64
	var transferred Fixed64
	input := []*transaction.UTXOTxInput{}
	output := []*transaction.TxOutput{}
	// construct transaction outputs
	for _, o := range batchOut {
		outputValue, err := StringToFixed64(o.Value)
		if err != nil {
			return nil, err
		}
		if outputValue <= perOutputFee {
			return nil, errors.New("token is not enough for transaction fee")
		}
		expected += outputValue
		address, err := ToScriptHash(o.Address)
		if err != nil {
			return nil, errors.New("invalid address")
		}
		tmp := &transaction.TxOutput{
			AssetID:     assetID,
			Value:       outputValue - perOutputFee,
			ProgramHash: address,
		}
		output = append(output, tmp)
	}

	// construct transaction inputs and changes
	coins := wallet.GetCoins()
	sorted := sortCoinsByValue(coins)
	for _, coinItem := range sorted {
		if coinItem.coin.Output.AssetID == assetID {
			input = append(input, coinItem.input)
			if coinItem.coin.Output.Value > expected {
				changes := &transaction.TxOutput{
					AssetID:     assetID,
					Value:       coinItem.coin.Output.Value - expected,
					ProgramHash: mainAccount.ProgramHash,
				}
				// if any, the changes output of transaction will be the last one
				output = append(output, changes)
				transferred += expected
				expected = 0
				break
			} else if coinItem.coin.Output.Value == expected {
				transferred += expected
				expected = 0
				break
			} else if coinItem.coin.Output.Value < expected {
				transferred += coinItem.coin.Output.Value
				expected = expected - coinItem.coin.Output.Value
			}
		}
	}
	if expected > 0 {
		return nil, errors.New("token is not enough")
	}

	// construct transaction
	txn, err := transaction.NewTransferAssetTransaction(input, output)
	if err != nil {
		return nil, err
	}
	txAttr := transaction.NewTxAttribute(transaction.Nonce, []byte(strconv.FormatInt(rand.Int63(), 10)))
	txn.Attributes = make([]*transaction.TxAttribute, 0)
	txn.Attributes = append(txn.Attributes, &txAttr)

	// get sorted singer's account
	var accounts sortedAccounts
	for ref, coin := range coins {
		for _, in := range input {
			if in.ReferTxID == ref.ReferTxID && in.ReferTxOutputIndex == ref.ReferTxOutputIndex {
				foundDuplicate := false
				for _, tmp := range accounts {
					// skip duplicated program hash
					if coin.Output.ProgramHash == tmp.ProgramHash {
						foundDuplicate = true
						break
					}
				}
				if !foundDuplicate {
					accounts = append(accounts, wallet.GetAccountByProgramHash(coin.Output.ProgramHash))
				}
			}
		}
	}
	sort.Sort(accounts)

	// sign transaction
	ctx := newContractContextWithoutProgramHashes(txn, len(accounts))
	i := 0
	for _, account := range accounts {
		signature, _ := signature.SignBySigner(txn, account)
		contract, _ := contract.CreateSignatureContract(account.PublicKey)
		ctx.MyAdd(contract, i, signature)
		i++
	}
	txn.SetPrograms(ctx.GetPrograms())

	return txn, nil
}

func signTransaction(signer *account.Account, tx *transaction.Transaction) error {
	signature, err := signature.SignBySigner(tx, signer)
	if err != nil {
		fmt.Println("SignBySigner failed")
		return err
	}
	transactionContract, err := contract.CreateSignatureContract(signer.PubKey())
	if err != nil {
		fmt.Println("CreateSignatureContract failed")
		return err
	}
	transactionContractContext := newContractContextWithoutProgramHashes(tx, 1)
	if err := transactionContractContext.AddContract(transactionContract, signer.PubKey(), signature); err != nil {
		fmt.Println("SaveContract failed")
		return err
	}
	tx.SetPrograms(transactionContractContext.GetPrograms())
	return nil
}

func newContractContextWithoutProgramHashes(data signature.SignableData, length int) *contract.ContractContext {
	return &contract.ContractContext{
		Data:       data,
		Codes:      make([][]byte, length),
		Parameters: make([][][]byte, length),
	}
}
