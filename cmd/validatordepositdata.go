// Copyright © 2019, 2020 Weald Technology Trading
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cmd

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"strings"

	"github.com/prysmaticlabs/go-ssz"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"github.com/wealdtech/ethdo/grpc"
	e2types "github.com/wealdtech/go-eth2-types/v2"
	util "github.com/wealdtech/go-eth2-util"
	string2eth "github.com/wealdtech/go-string2eth"
)

var validatorDepositDataValidatorAccount string
var validatorDepositDataWithdrawalAccount string
var validatorDepositDataWithdrawalPubKey string
var validatorDepositDataDepositValue string
var validatorDepositDataRaw bool
var validatorDepositDataForkVersion string
var validatorDepositDataLaunchpad bool

var validatorDepositDataCmd = &cobra.Command{
	Use:   "depositdata",
	Short: "Generate deposit data for one or more validators",
	Long: `Generate data for deposits to the Ethereum 1 validator contract.  For example:

    ethdo validator depositdata --validatoraccount=primary/validator --withdrawalaccount=primary/current --value="32 Ether"

If validatoraccount is provided with an account path it will generate deposit data for all matching accounts.

The information generated can be passed to ethereal to create a deposit from the Ethereum 1 chain.

In quiet mode this will return 0 if the the data can be generated correctly, otherwise 1.`,
	Run: func(cmd *cobra.Command, args []string) {
		ctx, cancel := context.WithTimeout(context.Background(), viper.GetDuration("timeout"))
		defer cancel()

		assert(validatorDepositDataValidatorAccount != "", "--validatoraccount is required")
		validatorWallet, validatorAccounts, err := walletAndAccountsFromPath(ctx, validatorDepositDataValidatorAccount)
		errCheck(err, "Failed to obtain validator accounts")
		assert(len(validatorAccounts) > 0, "Failed to obtain validator account")

		for _, validatorAccount := range validatorAccounts {
			outputIf(verbose, fmt.Sprintf("Creating deposit for %s/%s", validatorWallet.Name(), validatorAccount.Name()))
			pubKey, err := bestPublicKey(validatorAccount)
			errCheck(err, "Validator account does not provide a public key")
			outputIf(debug, fmt.Sprintf("Validator public key is %#x", pubKey.Marshal()))
		}

		assert(validatorDepositDataWithdrawalAccount != "" || validatorDepositDataWithdrawalPubKey != "", "--withdrawalaccount or --withdrawalpubkey is required")
		var withdrawalCredentials []byte
		if validatorDepositDataWithdrawalAccount != "" {
			_, withdrawalAccount, err := walletAndAccountFromPath(ctx, validatorDepositDataWithdrawalAccount)
			errCheck(err, "Failed to obtain withdrawal account")
			pubKey, err := bestPublicKey(withdrawalAccount)
			errCheck(err, "Withdrawal account does not provide a public key")
			outputIf(debug, fmt.Sprintf("Withdrawal public key is %#x", pubKey.Marshal()))
			withdrawalCredentials = util.SHA256(pubKey.Marshal())
			errCheck(err, "Failed to hash withdrawal credentials")
		} else {
			withdrawalPubKeyBytes, err := hex.DecodeString(strings.TrimPrefix(validatorDepositDataWithdrawalPubKey, "0x"))
			errCheck(err, "Invalid withdrawal public key")
			assert(len(withdrawalPubKeyBytes) == 48, "Public key should be 48 bytes")
			withdrawalPubKey, err := e2types.BLSPublicKeyFromBytes(withdrawalPubKeyBytes)
			errCheck(err, "Value supplied with --withdrawalpubkey is not a valid public key")
			withdrawalCredentials = util.SHA256(withdrawalPubKey.Marshal())
			errCheck(err, "Failed to hash withdrawal credentials")
		}
		// This is hard-coded, to allow deposit data to be generated without a connection to the beacon node.
		withdrawalCredentials[0] = byte(0) // BLS_WITHDRAWAL_PREFIX
		outputIf(debug, fmt.Sprintf("Withdrawal credentials are %#x", withdrawalCredentials))

		assert(validatorDepositDataDepositValue != "", "--depositvalue is required")
		val, err := string2eth.StringToGWei(validatorDepositDataDepositValue)
		errCheck(err, "Invalid value")
		// This is hard-coded, to allow deposit data to be generated without a connection to the beacon node.
		assert(val >= 1000000000, "deposit value must be at least 1 Ether") // MIN_DEPOSIT_AMOUNT

		// For each key, generate deposit data
		outputs := make([]string, 0)
		for _, validatorAccount := range validatorAccounts {
			validatorPubKey, err := bestPublicKey(validatorAccount)
			errCheck(err, "Validator account does not provide a public key")
			depositData := struct {
				PubKey                []byte `ssz-size:"48"`
				WithdrawalCredentials []byte `ssz-size:"32"`
				Value                 uint64
			}{
				PubKey:                validatorPubKey.Marshal(),
				WithdrawalCredentials: withdrawalCredentials,
				Value:                 val,
			}
			outputIf(debug, fmt.Sprintf("Deposit data:\n\tPublic key: %x\n\tWithdrawal credentials: %x\n\tValue: %d", depositData.PubKey, depositData.WithdrawalCredentials, depositData.Value))

			var forkVersion []byte
			if validatorDepositDataForkVersion != "" {
				forkVersion, err = hex.DecodeString(strings.TrimPrefix(validatorDepositDataForkVersion, "0x"))
				errCheck(err, fmt.Sprintf("Failed to decode fork version %s", validatorDepositDataForkVersion))
				assert(len(forkVersion) == 4, "Fork version must be exactly four bytes")
			} else {
				err := connect()
				errCheck(err, "Failed to connect to beacon node")
				config, err := grpc.FetchChainConfig(eth2GRPCConn)
				if err != nil {
					outputIf(!quiet, "Could not connect to beacon node; supply a connection with --connection or provide a fork version with --forkversion to generate a deposit")
					os.Exit(_exitFailure)
				}
				genesisForkVersion, exists := config["GenesisForkVersion"]
				assert(exists, "Failed to obtain genesis fork version")
				forkVersion = genesisForkVersion.([]byte)
			}
			outputIf(debug, fmt.Sprintf("Fork version is %x", forkVersion))

			domain := e2types.Domain(e2types.DomainDeposit, forkVersion, e2types.ZeroGenesisValidatorsRoot)
			outputIf(debug, fmt.Sprintf("Domain is %x", domain))
			signature, err := signStruct(validatorAccount, depositData, domain)
			errCheck(err, "Failed to generate deposit data signature")

			signedDepositData := struct {
				PubKey                []byte `ssz-size:"48"`
				WithdrawalCredentials []byte `ssz-size:"32"`
				Value                 uint64
				Signature             []byte `ssz-size:"96"`
			}{
				PubKey:                validatorPubKey.Marshal(),
				WithdrawalCredentials: withdrawalCredentials,
				Value:                 val,
				Signature:             signature.Marshal(),
			}
			if debug {
				fmt.Printf("Signed deposit data:\n")
				fmt.Printf(" Public key: %#x\n", signedDepositData.PubKey)
				fmt.Printf(" Withdrawal credentials: %#x\n", signedDepositData.WithdrawalCredentials)
				fmt.Printf(" Value: %d\n", signedDepositData.Value)
				fmt.Printf(" Signature: %#x\n", signedDepositData.Signature)
			}

			depositDataRoot, err := ssz.HashTreeRoot(signedDepositData)
			errCheck(err, "Failed to generate deposit data root")
			outputIf(debug, fmt.Sprintf("Deposit data root is %x", depositDataRoot))

			switch {
			case validatorDepositDataRaw:
				// Build a raw transaction by hand
				txData := []byte{0x22, 0x89, 0x51, 0x18}
				// Pointer to validator public key
				txData = append(txData, []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x80}...)
				// Pointer to withdrawal credentials
				txData = append(txData, []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0xe0}...)
				// Pointer to validator signature
				txData = append(txData, []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x20}...)
				// Deposit data root
				txData = append(txData, depositDataRoot[:]...)
				// Validator public key (pad to 32-byte boundary)
				txData = append(txData, []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x30}...)
				txData = append(txData, validatorPubKey.Marshal()...)
				txData = append(txData, []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}...)
				// Withdrawal credentials
				txData = append(txData, []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x20}...)
				txData = append(txData, withdrawalCredentials...)
				// Deposit signature
				txData = append(txData, []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x60}...)
				txData = append(txData, signedDepositData.Signature...)
				outputs = append(outputs, fmt.Sprintf("%#x", txData))
			case validatorDepositDataLaunchpad:
				depositMessage := struct {
					PubKey                []byte `ssz-size:"48"`
					WithdrawalCredentials []byte `ssz-size:"32"`
					Value                 uint64
				}{
					PubKey:                validatorPubKey.Marshal(),
					WithdrawalCredentials: withdrawalCredentials,
					Value:                 val,
				}
				depositMessageRoot, err := ssz.HashTreeRoot(depositMessage)
				errCheck(err, "Failed to generate deposit message root")
				outputs = append(outputs, fmt.Sprintf(`[{"pubkey":"%x","withdrawal_credentials":"%x","amount":%d,"signature":"%x","deposit_message_root":"%x","deposit_data_root":"%x","fork_version":"%x"}]`, signedDepositData.PubKey, signedDepositData.WithdrawalCredentials, val, signedDepositData.Signature, depositMessageRoot, depositDataRoot, forkVersion))
			default:
				outputs = append(outputs, fmt.Sprintf(`{"name":"Deposit for %s","account":"%s","pubkey":"%#x","withdrawal_credentials":"%#x","signature":"%#x","value":%d,"deposit_data_root":"%#x","version":2}`, fmt.Sprintf("%s/%s", validatorWallet.Name(), validatorAccount.Name()), fmt.Sprintf("%s/%s", validatorWallet.Name(), validatorAccount.Name()), signedDepositData.PubKey, signedDepositData.WithdrawalCredentials, signedDepositData.Signature, val, depositDataRoot))
			}
		}

		if quiet {
			os.Exit(0)
		}

		if len(outputs) == 1 {
			fmt.Printf("%s\n", outputs[0])
		} else {
			fmt.Printf("[")
			fmt.Print(strings.Join(outputs, ","))
			fmt.Println("]")
		}
	},
}

func init() {
	validatorCmd.AddCommand(validatorDepositDataCmd)
	validatorFlags(validatorDepositDataCmd)
	validatorDepositDataCmd.Flags().StringVar(&validatorDepositDataValidatorAccount, "validatoraccount", "", "Account of the account carrying out the validation")
	validatorDepositDataCmd.Flags().StringVar(&validatorDepositDataWithdrawalAccount, "withdrawalaccount", "", "Account of the account to which the validator funds will be withdrawn")
	validatorDepositDataCmd.Flags().StringVar(&validatorDepositDataWithdrawalPubKey, "withdrawalpubkey", "", "Public key of the account to which the validator funds will be withdrawn")
	validatorDepositDataCmd.Flags().StringVar(&validatorDepositDataDepositValue, "depositvalue", "", "Value of the amount to be deposited")
	validatorDepositDataCmd.Flags().BoolVar(&validatorDepositDataRaw, "raw", false, "Print raw deposit data transaction data")
	validatorDepositDataCmd.Flags().StringVar(&validatorDepositDataForkVersion, "forkversion", "", "Use a hard-coded fork version (default is to fetch it from the node)")
	validatorDepositDataCmd.Flags().BoolVar(&validatorDepositDataLaunchpad, "launchpad", false, "Print launchpad-compatible JSON")
}
