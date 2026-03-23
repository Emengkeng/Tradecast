//+------------------------------------------------------------------+
//| MT4Signal Receiver EA                                            |
//| Polls /pending/{symbol} and copies trades to this account.      |
//| Handles OPEN, MODIFY, CLOSE, PARTIAL signal types.              |
//+------------------------------------------------------------------+
#property copyright "MT4Signal"
#property version   "2.0"
#property strict

input string ServerURL      = "http://your-vps-ip:8080/pending";
input string APIKey         = "your-api-key-here";
input string SymbolToWatch  = "EURUSD";
input int    AccountID      = 0;        // Set this to your MT4 account number (AccountNumber())
input int    PollIntervalMs = 300;
input double LotMultiplier  = 1.0;   // Scale lot size (1.0 = same as source)
input int    Slippage       = 3;
input bool   CopyModify     = true;
input bool   CopyClose      = true;
input int    Magic          = 88001;

string lastProcessedKey = ""; // ticket_id + ":" + signal_type

//+------------------------------------------------------------------+
int OnInit() {
   EventSetMillisecondTimer(PollIntervalMs);
   Print("[Receiver] Started. Watching: ", SymbolToWatch, " Magic: ", Magic);
   return INIT_SUCCEEDED;
}

void OnDeinit(const int reason) {
   EventKillTimer();
}

void OnTimer() { PollAndAct(); }

//+------------------------------------------------------------------+
void PollAndAct() {
   string url = ServerURL + "/" + SymbolToWatch;
   string headers = "X-API-Key: " + APIKey + "\r\n";
   headers += "X-Account-ID: " + IntegerToString(AccountID > 0 ? AccountID : AccountNumber());
   char   get[], result[];
   string resHeaders;

   int res = WebRequest("GET", url, headers, 3000, get, result, resHeaders);
   if (res == -1) {
      Print("[Receiver] WebRequest error ", GetLastError(),
            ". Whitelist ", url, " in Tools > Options > Expert Advisors.");
      return;
   }
   if (res == 204) return; // no signal
   if (res != 200) {
      Print("[Receiver] Server error ", res);
      return;
   }

   string json = CharArrayToString(result);
   if (json == "") return;

   // Parse fields
   long   ticketID   = JSONGetLong(json, "ticket_id");
   string signalType = JSONGetString(json, "signal_type");
   string direction  = JSONGetString(json, "direction");
   double price      = JSONGetDouble(json, "price");
   double sl         = JSONGetDouble(json, "sl");
   double tp         = JSONGetDouble(json, "tp");
   double lot        = JSONGetDouble(json, "lot") * LotMultiplier;

   // Deduplicate — don't act twice on same signal
   string key = IntegerToString(ticketID) + ":" + signalType;
   if (key == lastProcessedKey) return;
   lastProcessedKey = key;

   Print("[Receiver] Got signal: ", signalType, " ", direction, " ", SymbolToWatch,
         " price=", price, " lot=", lot, " ticket=", ticketID);

   if (signalType == "OPEN") {
      OpenTrade(direction, price, sl, tp, lot, ticketID);
   } else if (signalType == "MODIFY" && CopyModify) {
      ModifyTrade(ticketID, sl, tp);
   } else if (signalType == "CLOSE" && CopyClose) {
      CloseTrade(ticketID);
   } else if (signalType == "PARTIAL" && CopyClose) {
      PartialClose(ticketID, lot);
   }
}

//+------------------------------------------------------------------+
void OpenTrade(string direction, double price, double sl, double tp, double lot, long sourceTicket) {
   int    cmd    = (direction == "BUY") ? OP_BUY : OP_SELL;
   double oprice = (cmd == OP_BUY) ? MarketInfo(SymbolToWatch, MODE_ASK) : MarketInfo(SymbolToWatch, MODE_BID);

   // Normalize lot
   double minLot  = MarketInfo(SymbolToWatch, MODE_MINLOT);
   double maxLot  = MarketInfo(SymbolToWatch, MODE_MAXLOT);
   double lotStep = MarketInfo(SymbolToWatch, MODE_LOTSTEP);
   lot = MathMax(minLot, MathMin(maxLot, MathRound(lot / lotStep) * lotStep));

   int ticket = OrderSend(SymbolToWatch, cmd, lot, oprice, Slippage, sl, tp,
                          "Copy#" + IntegerToString(sourceTicket), Magic, 0, clrNONE);
   if (ticket < 0) {
      Print("[Receiver] OrderSend failed: ", GetLastError());
   } else {
      Print("[Receiver] Trade opened. Ticket: ", ticket);
   }
}

void ModifyTrade(long sourceTicket, double sl, double tp) {
   for (int i = OrdersTotal() - 1; i >= 0; i--) {
      if (!OrderSelect(i, SELECT_BY_POS, MODE_TRADES)) continue;
      if (OrderMagicNumber() != Magic) continue;
      if (!StringContains(OrderComment(), IntegerToString(sourceTicket))) continue;

      if (!OrderModify(OrderTicket(), OrderOpenPrice(), sl, tp, 0, clrNONE)) {
         Print("[Receiver] OrderModify failed: ", GetLastError());
      } else {
         Print("[Receiver] Trade modified. SL=", sl, " TP=", tp);
      }
      return;
   }
}

void CloseTrade(long sourceTicket) {
   for (int i = OrdersTotal() - 1; i >= 0; i--) {
      if (!OrderSelect(i, SELECT_BY_POS, MODE_TRADES)) continue;
      if (OrderMagicNumber() != Magic) continue;
      if (!StringContains(OrderComment(), IntegerToString(sourceTicket))) continue;

      double cprice = (OrderType() == OP_BUY)
                       ? MarketInfo(SymbolToWatch, MODE_BID)
                       : MarketInfo(SymbolToWatch, MODE_ASK);
      if (!OrderClose(OrderTicket(), OrderLots(), cprice, Slippage, clrNONE)) {
         Print("[Receiver] OrderClose failed: ", GetLastError());
      } else {
         Print("[Receiver] Trade closed.");
      }
      return;
   }
}

void PartialClose(long sourceTicket, double newLot) {
   for (int i = OrdersTotal() - 1; i >= 0; i--) {
      if (!OrderSelect(i, SELECT_BY_POS, MODE_TRADES)) continue;
      if (OrderMagicNumber() != Magic) continue;
      if (!StringContains(OrderComment(), IntegerToString(sourceTicket))) continue;

      double closeAmount = OrderLots() - newLot;
      if (closeAmount <= 0) return;

      double lotStep = MarketInfo(SymbolToWatch, MODE_LOTSTEP);
      closeAmount = MathRound(closeAmount / lotStep) * lotStep;

      double cprice = (OrderType() == OP_BUY)
                       ? MarketInfo(SymbolToWatch, MODE_BID)
                       : MarketInfo(SymbolToWatch, MODE_ASK);
      if (!OrderClose(OrderTicket(), closeAmount, cprice, Slippage, clrNONE)) {
         Print("[Receiver] Partial close failed: ", GetLastError());
      } else {
         Print("[Receiver] Partial close done. Closed ", closeAmount, " lots.");
      }
      return;
   }
}

//+------------------------------------------------------------------+
// Minimal JSON parsing helpers (MQL4 has no native JSON)
long JSONGetLong(string json, string key) {
   string val = JSONGetRaw(json, key);
   return StringToInteger(val);
}

double JSONGetDouble(string json, string key) {
   string val = JSONGetRaw(json, key);
   if (val == "" || val == "null") return 0;
   return StringToDouble(val);
}

string JSONGetString(string json, string key) {
   string search = "\"" + key + "\":\"";
   int start = StringFind(json, search);
   if (start < 0) return "";
   start += StringLen(search);
   int end = StringFind(json, "\"", start);
   if (end < 0) return "";
   return StringSubstr(json, start, end - start);
}

string JSONGetRaw(string json, string key) {
   string search = "\"" + key + "\":";
   int start = StringFind(json, search);
   if (start < 0) return "";
   start += StringLen(search);
   // Skip whitespace
   while (start < StringLen(json) && StringGetCharacter(json, start) == ' ') start++;
   // Find end (comma or })
   int end = start;
   while (end < StringLen(json)) {
      ushort c = StringGetCharacter(json, end);
      if (c == ',' || c == '}') break;
      end++;
   }
   return StringSubstr(json, start, end - start);
}

bool StringContains(string haystack, string needle) {
   return StringFind(haystack, needle) >= 0;
}
