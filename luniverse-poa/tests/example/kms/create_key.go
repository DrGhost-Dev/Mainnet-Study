// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0
package main

import (
	"context"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/kms"
)

// KMSCreateKeyAPI defines the interface for the CreateKey function.
// We use this interface to test the function using a mocked service.
type KMSCreateKeyAPI interface {
	CreateKey(ctx context.Context,
		params *kms.CreateKeyInput,
		optFns ...func(*kms.Options)) (*kms.CreateKeyOutput, error)
	CreateAlias(ctx context.Context,
		params *kms.CreateAliasInput,
		optFns ...func(*kms.Options)) (*kms.CreateAliasOutput, error)
}

// MakeKey creates an AWS Key Management Service (AWS KMS) customer master key (CMK).
// Inputs:
//     c is the context of the method call, which includes the AWS Region.
//     api is the interface that defines the method call.
//     input defines the input arguments to the service call.
// Output:
//     If success, a CreateKeyOutput object containing the result of the service call and nil.
//     Otherwise, nil and an error from the call to CreateKey.
func MakeKey(c context.Context, api KMSCreateKeyAPI, input *kms.CreateKeyInput) (*kms.CreateKeyOutput, error) {
	return api.CreateKey(c, input)
}

func CreateAlias(c context.Context, api KMSCreateKeyAPI, input *kms.CreateAliasInput) (*kms.CreateAliasOutput, error) {
	return api.CreateAlias(c, input)
}

func main() {
	aliasName := "alias/lpoa_mainnet"

	cfg, err := config.LoadDefaultConfig(context.TODO())
	if err != nil {
		panic("configuration error, " + err.Error())
	}

	client := kms.NewFromConfig(cfg)

	listAliasInput := kms.ListAliasesInput{}
	aliases, err := client.ListAliases(context.TODO(), &listAliasInput)
	if err != nil {
		fmt.Println("Got error listing aliases:")
		fmt.Println(err)
		return
	}
	for _, a := range aliases.Aliases {
		if aliasName == *a.AliasName {
			fmt.Println("Already exist!")
			return
		}
	}
	fmt.Println("No item found. Continuing...")

	keyInput := &kms.CreateKeyInput{}
	createKeyResult, err := MakeKey(context.TODO(), client, keyInput)
	if err != nil {
		fmt.Println("Got error creating key:")
		fmt.Println(err)
		return
	}
	fmt.Println(*createKeyResult.KeyMetadata.KeyId)

	aliasInput := &kms.CreateAliasInput{AliasName: &aliasName, TargetKeyId: createKeyResult.KeyMetadata.KeyId}
	_, err = CreateAlias(context.TODO(), client, aliasInput)
	if err != nil {
		fmt.Println("Got error creating alias:")
		fmt.Println(err)
		return
	}
	fmt.Println("OK.")
}
