//+------------------------------------------------------------------+
//| MT4Signal Monitor EA                                             |
//| Detects OPEN/MODIFY/CLOSE/PARTIAL events and POSTs to server.   |
//| Signs every request with HMAC-SHA256 to prove authenticity.     |
//| Persists seen tickets to a CSV file to survive EA restarts.     |
//+------------------------------------------------------------------+
#property copyright "MT4Signal"
#property version   "2.0"
#property strict

input string ServerURL     = "http://your-vps-ip:8080/signal";
input string HMACSecret    = "your-hmac-secret-here"; // Must match SIGNAL_HMAC_SECRET on server
input int    PollIntervalMs = 300;
input string StateFile     = "mt4signal_state.csv";

struct TicketState {
   long   ticket;
   double lot;
   double sl;
   double tp;
};

TicketState knownTickets[];
int         ticketCount = 0;
datetime    lastPoll    = 0;

//+------------------------------------------------------------------+
int OnInit() {
   LoadStateFromFile();
   EventSetMillisecondTimer(PollIntervalMs);
   Print("[MT4Signal] Monitor EA started. Loaded ", ticketCount, " known tickets.");
   return INIT_SUCCEEDED;
}

void OnDeinit(const int reason) {
   EventKillTimer();
   Print("[MT4Signal] Monitor EA stopped.");
}

void OnTimer() { ScanTrades(); }

//+------------------------------------------------------------------+
void ScanTrades() {
   // Detect closed/partial trades first
   for (int i = ticketCount - 1; i >= 0; i--) {
      bool stillOpen = false;
      for (int j = OrdersTotal() - 1; j >= 0; j--) {
         if (!OrderSelect(j, SELECT_BY_POS, MODE_TRADES)) continue;
         if (OrderTicket() == knownTickets[i].ticket) {
            stillOpen = true;
            double currentLot = OrderLots();
            double currentSL  = OrderStopLoss();
            double currentTP  = OrderTakeProfit();

            // Detect MODIFY
            if (MathAbs(currentSL - knownTickets[i].sl) > 0.000001 ||
                MathAbs(currentTP - knownTickets[i].tp) > 0.000001) {
               SendSignal(OrderTicket(), "MODIFY", OrderSymbol(),
                          OrderType() == OP_BUY ? "BUY" : "SELL",
                          OrderOpenPrice(), currentSL, currentTP, currentLot);
               knownTickets[i].sl = currentSL;
               knownTickets[i].tp = currentTP;
               SaveStateToFile();
            }

            // Detect PARTIAL close (lot decreased)
            if (currentLot < knownTickets[i].lot - 0.001) {
               SendSignal(OrderTicket(), "PARTIAL", OrderSymbol(),
                          OrderType() == OP_BUY ? "BUY" : "SELL",
                          OrderOpenPrice(), currentSL, currentTP, currentLot);
               knownTickets[i].lot = currentLot;
               SaveStateToFile();
            }
            break;
         }
      }

      if (!stillOpen) {
         // Check history for close
         if (OrderSelect(knownTickets[i].ticket, SELECT_BY_TICKET, MODE_HISTORY)) {
            SendSignal(OrderTicket(), "CLOSE", OrderSymbol(),
                       OrderType() == OP_BUY ? "BUY" : "SELL",
                       OrderClosePrice(), OrderStopLoss(), OrderTakeProfit(), OrderLots());
         }
         RemoveKnownTicket(i);
         SaveStateToFile();
      }
   }

   // Detect new OPEN trades
   for (int j = OrdersTotal() - 1; j >= 0; j--) {
      if (!OrderSelect(j, SELECT_BY_POS, MODE_TRADES)) continue;
      if (OrderType() > OP_SELL) continue; // skip pending orders

      long ticket = OrderTicket();
      if (!IsKnownTicket(ticket)) {
         AddKnownTicket(ticket, OrderLots(), OrderStopLoss(), OrderTakeProfit());
         SendSignal(ticket, "OPEN", OrderSymbol(),
                    OrderType() == OP_BUY ? "BUY" : "SELL",
                    OrderOpenPrice(), OrderStopLoss(), OrderTakeProfit(), OrderLots());
         SaveStateToFile();
      }
   }
}

//+------------------------------------------------------------------+
void SendSignal(long ticket, string sigType, string symbol, string direction,
                double price, double sl, double tp, double lot) {

   string timestamp = TimeToString(TimeGMT(), TIME_DATE | TIME_MINUTES | TIME_SECONDS);
   // Convert to RFC3339 format: "2024-01-15T10:30:00Z"
   StringReplace(timestamp, ".", "-");
   StringReplace(timestamp, " ", "T");
   timestamp = timestamp + "Z";

   string signature = ComputeHMAC(HMACSecret,
      IntegerToString(ticket) + ":" + sigType + ":" + symbol + ":" + timestamp);

   string payload = "{";
   payload += "\"ticket_id\":" + IntegerToString(ticket) + ",";
   payload += "\"signal_type\":\"" + sigType + "\",";
   payload += "\"symbol\":\"" + symbol + "\",";
   payload += "\"direction\":\"" + direction + "\",";
   payload += "\"price\":" + DoubleToString(price, 8) + ",";
   if (sl > 0) payload += "\"sl\":" + DoubleToString(sl, 8) + ",";
   if (tp > 0) payload += "\"tp\":" + DoubleToString(tp, 8) + ",";
   payload += "\"lot\":" + DoubleToString(lot, 4) + ",";
   payload += "\"timestamp\":\"" + timestamp + "\"";
   payload += "}";

   string headers = "Content-Type: application/json\r\n";
   headers += "X-Signal-Signature: " + signature + "\r\n";
   headers += "X-Signal-Timestamp: " + timestamp;

   char postData[];
   char result[];
   string resultHeaders;
   StringToCharArray(payload, postData, 0, StringLen(payload));

   int res = WebRequest("POST", ServerURL, headers, 5000, postData, result, resultHeaders);
   if (res == -1) {
      int err = GetLastError();
      Print("[MT4Signal] WebRequest failed. Error: ", err,
            ". Make sure ", ServerURL, " is whitelisted in Tools > Options > Expert Advisors.");
      Print("[MT4Signal] Failed payload: ", payload);
   } else if (res != 200) {
      Print("[MT4Signal] Server returned ", res, ": ", CharArrayToString(result));
   } else {
      Print("[MT4Signal] Signal sent OK. Ticket:", ticket, " Type:", sigType, " Symbol:", symbol);
   }
}

//+------------------------------------------------------------------+
// HMAC-SHA256 implementation for MQL4
// Uses the same message format as the server: "ticket:type:symbol:timestamp"
string ComputeHMAC(string key, string message) {
   // MQL4 does not have native HMAC. We use a workaround via CryptEncode.
   // IMPORTANT: This requires the CryptEncode function available in MT4 build 1090+
   uchar keyBytes[], msgBytes[], resultBytes[];
   StringToCharArray(key, keyBytes, 0, StringLen(key));
   StringToCharArray(message, msgBytes, 0, StringLen(message));

   if (CryptEncode(CRYPT_HASH_SHA256_HMAC, msgBytes, keyBytes, resultBytes) <= 0) {
      Print("[MT4Signal] HMAC computation failed");
      return "";
   }

   // Convert to hex string
   string hexResult = "";
   for (int i = 0; i < ArraySize(resultBytes); i++) {
      hexResult += StringFormat("%02x", resultBytes[i]);
   }
   return hexResult;
}

//+------------------------------------------------------------------+
// Ticket state management
bool IsKnownTicket(long ticket) {
   for (int i = 0; i < ticketCount; i++)
      if (knownTickets[i].ticket == ticket) return true;
   return false;
}

void AddKnownTicket(long ticket, double lot, double sl, double tp) {
   ArrayResize(knownTickets, ticketCount + 1);
   knownTickets[ticketCount].ticket = ticket;
   knownTickets[ticketCount].lot    = lot;
   knownTickets[ticketCount].sl     = sl;
   knownTickets[ticketCount].tp     = tp;
   ticketCount++;
}

void RemoveKnownTicket(int index) {
   for (int i = index; i < ticketCount - 1; i++)
      knownTickets[i] = knownTickets[i + 1];
   ticketCount--;
   ArrayResize(knownTickets, ticketCount);
}

//+------------------------------------------------------------------+
// File persistence — survives EA restarts without re-firing signals
void SaveStateToFile() {
   int handle = FileOpen(StateFile, FILE_WRITE | FILE_CSV | FILE_COMMON);
   if (handle == INVALID_HANDLE) {
      Print("[MT4Signal] Cannot open state file for writing");
      return;
   }
   for (int i = 0; i < ticketCount; i++) {
      FileWrite(handle,
         IntegerToString(knownTickets[i].ticket),
         DoubleToString(knownTickets[i].lot, 4),
         DoubleToString(knownTickets[i].sl, 8),
         DoubleToString(knownTickets[i].tp, 8)
      );
   }
   FileClose(handle);
}

void LoadStateFromFile() {
   if (!FileIsExist(StateFile, FILE_COMMON)) return;
   int handle = FileOpen(StateFile, FILE_READ | FILE_CSV | FILE_COMMON);
   if (handle == INVALID_HANDLE) return;

   ticketCount = 0;
   ArrayResize(knownTickets, 0);

   while (!FileIsEnding(handle)) {
      string ticketStr = FileReadString(handle);
      if (ticketStr == "") break;
      double lot = StringToDouble(FileReadString(handle));
      double sl  = StringToDouble(FileReadString(handle));
      double tp  = StringToDouble(FileReadString(handle));

      long ticket = StringToInteger(ticketStr);
      // Only load tickets that are still actually open
      if (OrderSelect((int)ticket, SELECT_BY_TICKET, MODE_TRADES)) {
         AddKnownTicket(ticket, lot, sl, tp);
      }
   }
   FileClose(handle);
   Print("[MT4Signal] State loaded: ", ticketCount, " open tickets.");
}
