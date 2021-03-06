package core

import (
	"encoding/hex"
	"errors"
	"time"

	"github.com/kimitzu/kimitzu-go/pb"
	"github.com/OpenBazaar/wallet-interface"
	"github.com/golang/protobuf/proto"
	"github.com/golang/protobuf/ptypes"
)

// RefundOrder - refund buyer
func (n *OpenBazaarNode) RefundOrder(contract *pb.RicardianContract, records []*wallet.TransactionRecord) error {
	refundMsg := new(pb.Refund)
	orderID, err := n.CalcOrderID(contract.BuyerOrder)
	if err != nil {
		return err
	}
	refundMsg.OrderID = orderID
	ts, err := ptypes.TimestampProto(time.Now())
	if err != nil {
		return err
	}
	refundMsg.Timestamp = ts
	wal, err := n.Multiwallet.WalletForCurrencyCode(contract.BuyerOrder.Payment.Coin)
	if err != nil {
		return err
	}
	if contract.BuyerOrder.Payment.Method == pb.Order_Payment_MODERATED {
		var ins []wallet.TransactionInput
		var outValue int64
		for _, r := range records {
			if !r.Spent && r.Value > 0 {
				outpointHash, err := hex.DecodeString(r.Txid)
				if err != nil {
					return err
				}
				outValue += r.Value
				in := wallet.TransactionInput{OutpointIndex: r.Index, OutpointHash: outpointHash, Value: r.Value}
				ins = append(ins, in)
			}
		}

		refundAddress, err := wal.DecodeAddress(contract.BuyerOrder.RefundAddress)
		if err != nil {
			return err
		}
		output := wallet.TransactionOutput{
			Address: refundAddress,
			Value:   outValue,
		}

		chaincode, err := hex.DecodeString(contract.BuyerOrder.Payment.Chaincode)
		if err != nil {
			return err
		}
		mECKey, err := n.MasterPrivateKey.ECPrivKey()
		if err != nil {
			return err
		}
		vendorKey, err := wal.ChildKey(mECKey.Serialize(), chaincode, true)
		if err != nil {
			return err
		}
		redeemScript, err := hex.DecodeString(contract.BuyerOrder.Payment.RedeemScript)
		if err != nil {
			return err
		}

		signatures, err := wal.CreateMultisigSignature(ins, []wallet.TransactionOutput{output}, vendorKey, redeemScript, contract.BuyerOrder.RefundFee)
		if err != nil {
			return err
		}
		var sigs []*pb.BitcoinSignature
		for _, s := range signatures {
			pbSig := &pb.BitcoinSignature{Signature: s.Signature, InputIndex: s.InputIndex}
			sigs = append(sigs, pbSig)
		}
		refundMsg.Sigs = sigs
	} else {
		var outValue int64
		for _, r := range records {
			if r.Value > 0 {
				outValue += r.Value
			}
		}
		refundAddr, err := wal.DecodeAddress(contract.BuyerOrder.RefundAddress)
		if err != nil {
			return err
		}
		txid, err := wal.Spend(outValue, refundAddr, wallet.NORMAL, orderID, false)
		if err != nil {
			return err
		}
		txinfo := new(pb.Refund_TransactionInfo)
		txinfo.Txid = txid.String()
		txinfo.Value = uint64(outValue)
		refundMsg.RefundTransaction = txinfo
	}
	contract.Refund = refundMsg
	contract, err = n.SignRefund(contract)
	if err != nil {
		return err
	}
	n.SendRefund(contract.BuyerOrder.BuyerID.PeerID, contract)
	n.Datastore.Sales().Put(orderID, *contract, pb.OrderState_REFUNDED, true)
	return nil
}

// SignRefund - add signature to refund
func (n *OpenBazaarNode) SignRefund(contract *pb.RicardianContract) (*pb.RicardianContract, error) {
	serializedRefund, err := proto.Marshal(contract.Refund)
	if err != nil {
		return contract, err
	}
	s := new(pb.Signature)
	s.Section = pb.Signature_REFUND
	guidSig, err := n.IpfsNode.PrivateKey.Sign(serializedRefund)
	if err != nil {
		return contract, err
	}
	s.SignatureBytes = guidSig
	contract.Signatures = append(contract.Signatures, s)
	return contract, nil
}

// VerifySignaturesOnRefund - verify signatures on refund
func (n *OpenBazaarNode) VerifySignaturesOnRefund(contract *pb.RicardianContract) error {
	if err := verifyMessageSignature(
		contract.Refund,
		contract.VendorListings[0].VendorID.Pubkeys.Identity,
		contract.Signatures,
		pb.Signature_REFUND,
		contract.VendorListings[0].VendorID.PeerID,
	); err != nil {
		switch err.(type) {
		case noSigError:
			return errors.New("contract does not contain a signature for the refund")
		case invalidSigError:
			return errors.New("vendor's guid signature on contact failed to verify")
		case matchKeyError:
			return errors.New("public key in order does not match reported vendor ID")
		default:
			return err
		}
	}
	return nil
}
