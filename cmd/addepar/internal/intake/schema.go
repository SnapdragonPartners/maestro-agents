package intake

// The tools' input schemas, offered to the model. They describe the shape;
// code checks every value (statement.Record.Validate, then the verifier), so
// a schema the model ignores admits nothing.

const amountSchema = `{"type":"object","properties":{
"label":{"type":"string","description":"the label as printed, without a glued footnote digit"},
"amount":{"type":"string","description":"signed decimal string as printed: no symbols or separators; parentheses are negative"},
"page":{"type":"integer","minimum":1}},
"required":["label","amount","page"],"additionalProperties":false}`

const lineSchema = `{"type":"object","properties":{
"kind":{"type":"string","enum":["movement","subtotal"]},
"label":{"type":"string"},
"amount":{"type":"string"},
"page":{"type":"integer","minimum":1}},
"required":["kind","label","amount","page"],"additionalProperties":false}`

const textSchema = `{"type":"object","properties":{"text":{"type":"string"},"page":{"type":"integer","minimum":1}},"required":["text","page"],"additionalProperties":false}`

const recordSchema = `{"type":"object","properties":{
"investor":` + textSchema + `,
"fund":` + textSchema + `,
"period":{"type":"object","properties":{"end":{"type":"string","description":"YYYY-MM-DD"},"label":{"type":"string","description":"the printed period text"},"page":{"type":"integer","minimum":1}},"required":["end","label","page"],"additionalProperties":false},
"currency":{"type":"object","properties":{"code":{"type":"string","description":"ISO 4217 code"},"evidence":{"type":"string","description":"the printed text stating the currency; omit for a bare $"},"page":{"type":"integer","minimum":1}},"required":["code"],"additionalProperties":false},
"precision":{"type":"string","enum":["cents","whole"]},
"column":{"type":"object","properties":{"label":{"type":"string"},"section":{"type":"string"},"reason":{"type":"string"},"page":{"type":"integer","minimum":1}},"required":["label","reason","page"],"additionalProperties":false},
"dash_zero":{"type":"string","description":"why a dash means zero in this table; omit otherwise"},
"beginning":` + amountSchema + `,
"lines":{"type":"array","items":` + lineSchema + `},
"ending":` + amountSchema + `,
"fund_total":` + amountSchema + `,
"repeats":{"type":"array","items":` + amountSchema + `}},
"required":["investor","fund","period","currency","precision","column","ending"],"additionalProperties":false}`

const submitSchema = `{"type":"object","properties":{"records":{"type":"array","minItems":1,"description":"one record per investor and fund in the document","items":` + recordSchema + `}},"required":["records"],"additionalProperties":false}`

const emptySchema = `{"type":"object","properties":{},"additionalProperties":false}`

const setAsideSchema = `{"type":"object","properties":{
"document_type":{"type":"string","enum":["capital_call","distribution_notice","k1","cover_letter","other"]},
"reason":{"type":"string"}},
"required":["document_type","reason"],"additionalProperties":false}`

const manualReviewSchema = `{"type":"object","properties":{"reason":{"type":"string"}},"required":["reason"],"additionalProperties":false}`
