"use strict";
Object.defineProperty(exports, "__esModule", { value: true });
const expressionParser_1 = require("./src/compiler/expressionParser");
const funcMaps = new Map();
funcMaps.set('add', { name: 'add', args: ['int', 'int'], returns: ['int'] });
const result = (0, expressionParser_1.inferExpressionType)('add 10 20', new Map(), [], undefined, funcMaps);
console.log(result);
//# sourceMappingURL=test_infer.js.map