package service

import (
	"github.com/stretchr/testify/require"
	"testing"
)

const excelBPSToolPolicyResponse = "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_policy\",\"status\":\"completed\",\"model\":\"gpt-6-astra\",\"output\":[],\"usage\":{\"input_tokens\":10,\"output_tokens\":2}}}\n\n"

func TestExcelBPSOmitUnsupportedToolsRequiresExplicitOptIn(t *testing.T) {
	var missing *Account
	require.False(t, missing.IsExcelBPSOmitUnsupportedToolsEnabled())
	for _, value := range []any{nil, false, "true", 1, true} {
		account := excelAccount()
		account.Extra[ExcelBPSOmitUnsupportedToolsKey] = value
		require.Equal(t, value == true, account.IsExcelBPSOmitUnsupportedToolsEnabled())
		account.Extra["openai_excel_bps"] = false
		require.False(t, account.IsExcelBPSOmitUnsupportedToolsEnabled())
		account.Extra["openai_excel_bps"] = true
		account.Type = AccountTypeAPIKey
		require.False(t, account.IsExcelBPSOmitUnsupportedToolsEnabled())
	}
}
